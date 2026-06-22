// Package itip is the client-side scheduling orchestration core: the layer that
// turns inbound iTIP/iMIP invitations into calendar actions and the user's
// accept/decline into outbound iMIP replies. It wires the parse/generate/compose
// engine (internal/scheduling) to the backend ports (internal/calendar.Writer
// for the calendar store, internal/smtp.Sender for outbound mail).
//
// The split is deliberate. internal/scheduling owns the iCalendar/RFC 5546/6047
// mechanics — Parse a raw iMIP message into an [scheduling.Invite], generate a
// REQUEST/REPLY/CANCEL VCALENDAR, and Compose it into an RFC 822 message. This
// package owns the *orchestration*: given a raw REQUEST, it maps the invite onto
// a neutral [calendar.Event] and creates it; given the user's decision, it builds
// the REPLY and submits it to the organizer. Everything here is a pure function
// over the engine plus the injected ports, so each step is independently testable
// with fakes and the clock is always passed in (no time.Now, no randomness).
//
// This is the ORCHESTRATION half of MB720-10. Deliberately DEFERRED to the
// follow-up "trigger" issue (build the functions, not the loop):
//
//   - the inbound feed — where raw mail is pulled from the mailbox and routed to
//     [ProcessRequest], and where the user's accept/decline is routed to
//     [Respond];
//   - the RFC 6638 capability switch — delegating to a scheduling-aware CalDAV
//     server instead of running this engine when the backend does its own iTIP;
//   - IMAP-keyword idempotency — marking a processed invite so it is not acted on
//     twice;
//   - UID-correlated updates — locating an existing tentative event by its
//     iCalendar UID and updating it (a re-sent REQUEST, a CANCEL) rather than
//     always creating a new one. [ProcessRequest] today always creates.
package itip

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hstern/go-jscalendar"
	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

// ErrNotRequest is returned by ProcessRequest when the message is a valid iMIP
// scheduling object but not a METHOD:REQUEST (e.g. a REPLY or CANCEL). Together
// with scheduling.ErrNoCalendar (ordinary mail), it lets a mailbox-scanning loop
// skip messages it should not turn into tentative events without treating them as
// errors.
var ErrNotRequest = errors.New("itip: message is not a scheduling REQUEST")

// StatusTentative is the [calendar.Event.Status] marker for an inbound REQUEST
// the user has not yet answered: a tentatively-held event surfaced on the
// calendar so the time is visibly blocked while it awaits an accept/decline. It
// mirrors the iCalendar PARTSTAT/STATUS notion of "tentative" in the neutral
// event shape.
const StatusTentative = "tentative"

// EventFromInvite maps a parsed iTIP invite (typically a REQUEST) onto a
// backend-neutral [calendar.Event]. It performs no I/O.
//
// Field mapping (scheduling.Invite -> calendar.Event):
//
//   - Summary    -> Title
//   - Start/End  -> Start/TimeZone/Duration (via SetUTCTimes; the invite's UTC
//     instants become an "Etc/UTC" wall-clock JSCalendar time)
//   - UID        -> UID        (the stable correlation key across the invite's life)
//   - Sequence   -> Sequence   (int -> uint; a negative wire SEQUENCE clamps to 0)
//   - Organizer  -> the "owner"-role JSCalendar Participant
//   - Attendees  -> the "attendee"-role JSCalendar Participants, each carrying its
//     PARTSTAT mapped to the JSCalendar participationStatus vocabulary
//   - RecurrenceID -> RecurrenceID (+RecurrenceIDTimeZone "Etc/UTC"), parsed from
//     the iCalendar RECURRENCE-ID value to the occurrence's original-start instant;
//     nil for a master REQUEST. When set, IsOverride is marked true: the invite
//     addresses a single overridden occurrence of a series, not the whole series.
//   - (constant) -> Status = StatusTentative ("tentative")
//
// The remaining [calendar.Event] fields (ID, CalendarID, Locations, Description,
// ShowWithoutTime, Created) are left zero: ID/CalendarID are assigned by the store
// on create, and the others are not modelled on an [scheduling.Invite]. Status is
// always set to the tentative marker because an inbound REQUEST is a proposal the
// user has not yet answered.
func EventFromInvite(inv *scheduling.Invite) calendar.Event {
	if inv == nil {
		return calendar.Event{}
	}

	attendees := make([]jscalendar.Participant, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		attendees = append(attendees, calendar.NewParticipant(
			a.Name, a.Email, partStatToJSCal(a.PartStat), "attendee"))
	}

	var ev calendar.Event
	ev.UID = inv.UID
	ev.Title = inv.Summary
	ev.Sequence = sequenceToUint(inv.Sequence)
	ev.Status = StatusTentative
	ev.SetUTCTimes(inv.Start, inv.End)

	var organizer *jscalendar.Participant
	if inv.Organizer.Email != "" || inv.Organizer.Name != "" {
		o := calendar.NewParticipant(inv.Organizer.Name, inv.Organizer.Email, "", "owner")
		organizer = &o
	}
	ev.SetOrganizerAttendees(organizer, attendees)

	if rid, ok := parseRecurrenceID(inv.RecurrenceID); ok {
		ldt := localDateTimeUTC(rid)
		ev.RecurrenceID = &ldt
		ev.RecurrenceIDTimeZone = "Etc/UTC"
		ev.IsOverride = true
	}
	return ev
}

// InviteFromEvent maps a stored backend-neutral [calendar.Event] onto a
// scheduling [Invite] carrying just enough for a REPLY. It is the inverse of
// [EventFromInvite] for the fields a reply needs, and performs no I/O.
//
// It exists so the user can answer an invitation from the STORED event (the only
// thing the future accept/decline Graph endpoints have — an event id) rather than
// from the original raw REQUEST mail, which is not retained.
//
// Field mapping (calendar.Event -> scheduling.Invite):
//
//   - UID        -> UID        (the correlation key the REPLY echoes)
//   - Title      -> Summary
//   - StartTime  -> Start      (RFC 5546 §3.2.3 requires DTSTART in a REPLY)
//   - EndTime    -> End
//   - Organizer  -> Organizer  (the "owner"-role Participant -> scheduling.Address;
//     the address the REPLY is sent to)
//   - Attendees  -> Attendees  (each "attendee"-role Participant -> scheduling.Attendee,
//     its JSCalendar participationStatus mapped back to the iCalendar PARTSTAT)
//
// Method is set to [scheduling.MethodRequest] because the returned Invite stands
// in for the original request the user is replying to.
//
// The event's SEQUENCE is carried through (uint -> int), so a re-issued
// REQUEST/CANCEL or a REPLY addresses the event at its true revision.
//
// RECURRENCE-ID: when the event addresses a single overridden instance of a
// recurring series (ev.RecurrenceID is non-nil — e.g. an occurrence surfaced by
// the InstanceReader), the Invite is stamped with that RECURRENCE-ID via
// [scheduling.Invite.SetRecurrenceID], so the REPLY/REQUEST/CANCEL it backs targets
// the one occurrence rather than the master series. A nil RecurrenceID leaves the
// invite addressing the master.
func InviteFromEvent(ev calendar.Event) *scheduling.Invite {
	evAttendees := ev.Attendees()
	attendees := make([]scheduling.Attendee, 0, len(evAttendees))
	for _, a := range evAttendees {
		attendees = append(attendees, scheduling.Attendee{
			Address:  scheduling.Address{Name: a.Name, Email: calendar.ParticipantEmail(a)},
			PartStat: jsCalToPartStat(a.ParticipationStatus),
		})
	}

	var organizer scheduling.Address
	if org, ok := ev.Organizer(); ok {
		organizer = scheduling.Address{Name: org.Name, Email: calendar.ParticipantEmail(org)}
	}

	inv := &scheduling.Invite{
		Method:    scheduling.MethodRequest,
		UID:       ev.UID,
		Summary:   ev.Title,
		Start:     ev.StartTime(),
		End:       ev.EndTime(),
		Sequence:  int(ev.Sequence),
		Organizer: organizer,
		Attendees: attendees,
	}
	if ev.RecurrenceID != nil {
		inv.SetRecurrenceID(recurrenceIDToTime(ev.RecurrenceID, ev.RecurrenceIDTimeZone))
	}
	return inv
}

// recurrenceIDLayouts are the iCalendar RECURRENCE-ID value forms EventFromInvite
// parses back to an instant: the basic UTC DATE-TIME ("Z"), a floating/local
// DATE-TIME (no zone — interpreted as UTC, since the neutral Event carries UTC),
// and a DATE-only value. The CalDAV adapter and SetRecurrenceID both emit the UTC
// DATE-TIME form, so that is tried first.
var recurrenceIDLayouts = []string{
	"20060102T150405Z",
	"20060102T150405",
	"20060102",
}

// parseRecurrenceID parses an iCalendar RECURRENCE-ID property value into its
// instant (in UTC), reporting ok=false for an empty value or one in no recognized
// form. It is the inverse of the DATE-TIME [scheduling.Invite.SetRecurrenceID]
// writes and tolerates the floating and DATE-only shapes a peer might send.
func parseRecurrenceID(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range recurrenceIDLayouts {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ProcessRequest is the inbound-REQUEST orchestration: it parses a raw iMIP
// message, verifies it is a METHOD:REQUEST, maps it onto a tentative
// [calendar.Event] via [EventFromInvite], and creates that event in the named
// calendar through the [calendar.Writer]. It returns the created event (stamped
// by the store with its assigned ID).
//
// A non-REQUEST method (a REPLY, a CANCEL, or anything else) is rejected with a
// clear error: this path surfaces invitations as tentative events, and the other
// methods belong to different flows (a REPLY is organizer-side response tracking;
// a CANCEL is a withdrawal, handled once UID-correlated updates land).
//
// UID correlation: if w also implements [calendar.Finder], a re-sent REQUEST is
// matched to the event an earlier REQUEST created (by UID) and updates it in place
// when it carries a higher SEQUENCE, rather than creating a duplicate; an equal or
// lower SEQUENCE is treated as a duplicate/stale delivery and left untouched. A
// backend without Finder always creates (the first-cut behavior).
//
// Single-instance REQUEST: when the REQUEST carries a RECURRENCE-ID (an organizer
// re-inviting one occurrence of a series) and the backend implements both
// [calendar.Finder] and [calendar.InstanceWriter], the invite is recorded as a
// per-occurrence override against the located series master rather than as a
// standalone event. A backend lacking InstanceWriter falls back to the standard
// create/update path, surfacing the occurrence as its own tentative event.
func ProcessRequest(ctx context.Context, w calendar.Writer, calendarID string, raw []byte) (calendar.Event, error) {
	inv, err := scheduling.Parse(raw)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("itip: parse request: %w", err)
	}
	if inv.Method != scheduling.MethodRequest {
		return calendar.Event{}, fmt.Errorf("%w (method %q)", ErrNotRequest, inv.Method)
	}

	event := EventFromInvite(inv)

	// Single-instance REQUEST: an override for one occurrence of an existing series.
	// Locate the master by UID and record the override against it, leaving the
	// series rule and the other occurrences intact.
	if event.RecurrenceID != nil {
		f, hasFinder := w.(calendar.Finder)
		iw, hasInstanceWriter := w.(calendar.InstanceWriter)
		if hasFinder && hasInstanceWriter {
			master, found, err := f.FindEventByUID(ctx, calendarID, inv.UID)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("itip: find series master by uid: %w", err)
			}
			if found {
				stored, err := iw.WriteInstanceOverride(ctx, master.ID, event)
				if err != nil {
					return calendar.Event{}, fmt.Errorf("itip: write instance override: %w", err)
				}
				return stored, nil
			}
		}
	}

	// UID correlation: when the backend can locate the event a prior REQUEST created,
	// a re-sent REQUEST updates it in place rather than creating a duplicate. Only a
	// strictly higher SEQUENCE is a new revision; an equal or lower one is a
	// duplicate delivery or a stale resend, so the stored event (and the attendee's
	// own PARTSTAT on it) is kept untouched.
	if f, ok := w.(calendar.Finder); ok {
		existing, found, err := f.FindEventByUID(ctx, calendarID, inv.UID)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("itip: find event by uid: %w", err)
		}
		if found {
			if sequenceToUint(inv.Sequence) <= existing.Sequence {
				return existing, nil
			}
			event.ID = existing.ID
			updated, err := w.UpdateEvent(ctx, event)
			if err != nil {
				return calendar.Event{}, fmt.Errorf("itip: update event: %w", err)
			}
			return updated, nil
		}
	}

	created, err := w.CreateEvent(ctx, calendarID, event)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("itip: create event: %w", err)
	}
	return created, nil
}

// Respond is the user's-decision orchestration: it parses the original raw iMIP
// REQUEST, builds the METHOD:REPLY iMIP message conveying the responding
// attendee's participation status (via [scheduling.ComposeReply]), and submits it
// to the organizer through the [smtp.Sender] — from the attendee's address, to
// the organizer's.
//
// partStat is the user's decision ([scheduling.PartStatAccepted],
// PartStatDeclined, or PartStatTentative). date is passed in so the composed
// message is deterministic (no time.Now). It returns an error if the original
// message cannot be parsed, carries no organizer to reply to, the REPLY cannot be
// composed, or the send fails.
func Respond(ctx context.Context, sender smtp.Sender, attendee scheduling.Address, partStat scheduling.PartStat, raw []byte, date time.Time) error {
	req, err := scheduling.Parse(raw)
	if err != nil {
		return fmt.Errorf("itip: parse request: %w", err)
	}
	if req.Organizer.Email == "" {
		return fmt.Errorf("itip: respond: request has no organizer to reply to")
	}

	msg, err := scheduling.ComposeReply(req, attendee, partStat, date)
	if err != nil {
		return fmt.Errorf("itip: compose reply: %w", err)
	}

	if err := sender.Send(ctx, attendee.Email, []string{req.Organizer.Email}, msg); err != nil {
		return fmt.Errorf("itip: send reply: %w", err)
	}
	return nil
}

// RespondToEvent is the event-based analog of [Respond]: it lets the responding
// attendee answer an invitation from the STORED [calendar.Event] rather than from
// the original raw REQUEST mail (which is not retained). It builds the reply
// invite via [InviteFromEvent], composes the METHOD:REPLY iMIP message conveying
// the attendee's participation status (via [scheduling.ComposeReply]), and submits
// it to the organizer through the [smtp.Sender] — from the attendee's address, to
// the organizer's.
//
// attendee is the responding mailbox owner; the endpoint layer determines who
// "me" is, so the caller supplies it. partStat is the user's decision
// ([scheduling.PartStatAccepted], PartStatDeclined, or PartStatTentative). date is
// passed in so the composed message is deterministic (no time.Now). It returns an
// error if the event carries no organizer to reply to, the REPLY cannot be
// composed, or the send fails.
//
// When the stored event addresses a single overridden occurrence of a series
// (ev.RecurrenceID set), [InviteFromEvent] threads the RECURRENCE-ID through, so
// the REPLY targets that one occurrence at the stored event's SEQUENCE; a master
// event replies for the whole series.
func RespondToEvent(ctx context.Context, sender smtp.Sender, ev calendar.Event, attendee scheduling.Address, partStat scheduling.PartStat, date time.Time) error {
	inv := InviteFromEvent(ev)
	if inv.Organizer.Email == "" {
		return fmt.Errorf("itip: respond to event: event has no organizer to reply to")
	}

	msg, err := scheduling.ComposeReply(inv, attendee, partStat, date)
	if err != nil {
		return fmt.Errorf("itip: compose reply: %w", err)
	}

	if err := sender.Send(ctx, attendee.Email, []string{inv.Organizer.Email}, msg); err != nil {
		return fmt.Errorf("itip: send reply: %w", err)
	}
	return nil
}

// Invite is the organizer-side orchestration: it builds a METHOD:REQUEST iMIP
// message for the given invite (via [scheduling.Request] + [scheduling.Compose])
// and submits it to every attendee through the [smtp.Sender], from the
// organizer's address.
//
// It is the symmetric counterpart of [Respond]: where Respond sends an
// attendee's REPLY to the organizer, Invite sends the organizer's REQUEST to the
// attendees. date is passed in for deterministic output. It returns an error if
// inv is nil, the REQUEST cannot be built (no UID/organizer/attendees), it cannot
// be composed, or the send fails.
func Invite(ctx context.Context, sender smtp.Sender, organizer scheduling.Address, inv *scheduling.Invite, date time.Time) error {
	if inv == nil {
		return fmt.Errorf("itip: invite: nil invite")
	}

	ics, err := scheduling.Request(*inv)
	if err != nil {
		return fmt.Errorf("itip: build request: %w", err)
	}

	to := make([]scheduling.Address, 0, len(inv.Attendees))
	rcpt := make([]string, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		if a.Email == "" {
			continue
		}
		to = append(to, a.Address)
		rcpt = append(rcpt, a.Email)
	}
	if len(rcpt) == 0 {
		return fmt.Errorf("itip: invite: no attendee addresses to send to")
	}

	subject := "Invitation: " + inv.Summary
	msg, err := scheduling.Compose(organizer, to, subject, scheduling.MethodRequest, ics, date)
	if err != nil {
		return fmt.Errorf("itip: compose request: %w", err)
	}

	if err := sender.Send(ctx, organizer.Email, rcpt, msg); err != nil {
		return fmt.Errorf("itip: send request: %w", err)
	}
	return nil
}

// Cancel is the organizer-side withdrawal: it builds a METHOD:CANCEL iMIP message
// for the given invite (via [scheduling.Cancel] + [scheduling.Compose]) and submits
// it to every attendee through the [smtp.Sender], from the organizer's address.
//
// It is the counterpart of [Invite] for a deleted event: where Invite mails the
// REQUEST, Cancel withdraws it (STATUS:CANCELLED, a bumped SEQUENCE). date is passed
// in for deterministic output. It returns an error if inv is nil, the CANCEL cannot
// be built (no UID/organizer), it cannot be composed, or the send fails.
func Cancel(ctx context.Context, sender smtp.Sender, organizer scheduling.Address, inv *scheduling.Invite, date time.Time) error {
	if inv == nil {
		return fmt.Errorf("itip: cancel: nil invite")
	}

	ics, err := scheduling.Cancel(*inv)
	if err != nil {
		return fmt.Errorf("itip: build cancel: %w", err)
	}

	to := make([]scheduling.Address, 0, len(inv.Attendees))
	rcpt := make([]string, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		if a.Email == "" {
			continue
		}
		to = append(to, a.Address)
		rcpt = append(rcpt, a.Email)
	}
	if len(rcpt) == 0 {
		return fmt.Errorf("itip: cancel: no attendee addresses to send to")
	}

	subject := "Cancelled: " + inv.Summary
	msg, err := scheduling.Compose(organizer, to, subject, scheduling.MethodCancel, ics, date)
	if err != nil {
		return fmt.Errorf("itip: compose cancel: %w", err)
	}

	if err := sender.Send(ctx, organizer.Email, rcpt, msg); err != nil {
		return fmt.Errorf("itip: send cancel: %w", err)
	}
	return nil
}

// sequenceToUint maps an iCalendar SEQUENCE (a signed int on the scheduling
// Invite) onto the JSCalendar Sequence (uint), clamping a negative value to 0.
// RFC 5545 SEQUENCE is a non-negative integer, so a negative value is a
// malformed wire input and 0 is the safe revision floor.
func sequenceToUint(seq int) uint {
	if seq < 0 {
		return 0
	}
	return uint(seq)
}

// partStatToJSCal maps an iCalendar PARTSTAT to the JSCalendar
// participationStatus vocabulary; jsCalToPartStat is the inverse. An unmapped
// value yields "".
func partStatToJSCal(p scheduling.PartStat) string {
	switch p {
	case scheduling.PartStatAccepted:
		return "accepted"
	case scheduling.PartStatDeclined:
		return "declined"
	case scheduling.PartStatTentative:
		return "tentative"
	case scheduling.PartStatNeedsAction:
		return "needs-action"
	default:
		return ""
	}
}

func jsCalToPartStat(s string) scheduling.PartStat {
	switch s {
	case "accepted":
		return scheduling.PartStatAccepted
	case "declined":
		return scheduling.PartStatDeclined
	case "tentative":
		return scheduling.PartStatTentative
	case "needs-action":
		return scheduling.PartStatNeedsAction
	default:
		return ""
	}
}

// localDateTimeUTC builds a JSCalendar LocalDateTime from a UTC instant: the
// wall-clock fields of t.UTC(), paired with a "Etc/UTC" RecurrenceIDTimeZone by
// the caller. It mirrors the construction the frozen SetUTCTimes uses for Start.
func localDateTimeUTC(t time.Time) jscalendar.LocalDateTime {
	t = t.UTC()
	return jscalendar.LocalDateTime{
		Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
	}
}

// recurrenceIDToTime resolves a JSCalendar RecurrenceID LocalDateTime + its time
// zone back to a UTC instant — the inverse of localDateTimeUTC for the scheduling
// engine's time.Time-based SetRecurrenceID. An empty or unrecognized zone is
// treated as UTC.
func recurrenceIDToTime(ldt *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId) time.Time {
	if ldt == nil {
		return time.Time{}
	}
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(string(tz)); err == nil {
			loc = l
		}
	}
	return time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
		ldt.Hour, ldt.Minute, ldt.Second, 0, loc).UTC()
}

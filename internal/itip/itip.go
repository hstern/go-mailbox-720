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
//   - Summary    -> Subject
//   - Start      -> Start
//   - End        -> End
//   - UID        -> UID        (the stable correlation key across the invite's life)
//   - Organizer  -> Organizer  (scheduling.Address -> calendar.Address)
//   - Attendees  -> Attendees  (each Attendee.Address -> calendar.Address; the
//     per-attendee PARTSTAT is dropped — the neutral Event carries only the
//     roster of addresses, not their individual participation status)
//   - (constant) -> Status = StatusTentative ("tentative")
//
// The remaining [calendar.Event] fields (ID, CalendarID, Location, Body,
// IsAllDay, CreatedAt) are left zero: ID/CalendarID are assigned by the store on
// create, and the others are not modelled on an [scheduling.Invite]. Status is
// always set to the tentative marker because an inbound REQUEST is a proposal the
// user has not yet answered.
func EventFromInvite(inv *scheduling.Invite) calendar.Event {
	if inv == nil {
		return calendar.Event{}
	}

	attendees := make([]calendar.Attendee, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		ca := toCalendarAddress(a.Address)
		attendees = append(attendees, calendar.Attendee{
			Name:   ca.Name,
			Email:  ca.Email,
			Status: partStatToNeutral(a.PartStat),
		})
	}

	return calendar.Event{
		UID:       inv.UID,
		Subject:   inv.Summary,
		Start:     inv.Start,
		End:       inv.End,
		Sequence:  inv.Sequence,
		Organizer: toCalendarAddress(inv.Organizer),
		Attendees: attendees,
		Status:    StatusTentative,
	}
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
//   - Subject    -> Summary
//   - Start      -> Start      (RFC 5546 §3.2.3 requires DTSTART in a REPLY)
//   - End        -> End
//   - Organizer  -> Organizer  (calendar.Address -> scheduling.Address; the
//     address the REPLY is sent to)
//   - Attendees  -> Attendees  (each calendar.Address -> scheduling.Attendee with
//     an empty PARTSTAT; per-attendee participation status is not stored on the
//     neutral Event, so it cannot be reconstructed here)
//
// Method is set to [scheduling.MethodRequest] because the returned Invite stands
// in for the original request the user is replying to.
//
// The event's SEQUENCE is carried through, so a re-issued REQUEST/CANCEL or a REPLY
// addresses the event at its true revision.
//
// FIRST-CUT LIMITATION: the neutral Event does not carry an iCalendar RECURRENCE-ID,
// so it cannot be reconstructed here (RecurrenceID is empty) — this Invite addresses
// the master event, not a single overridden instance of a recurring series (a
// follow-up once the store models per-instance overrides).
func InviteFromEvent(ev calendar.Event) *scheduling.Invite {
	attendees := make([]scheduling.Attendee, 0, len(ev.Attendees))
	for _, a := range ev.Attendees {
		attendees = append(attendees, scheduling.Attendee{
			Address:  scheduling.Address{Name: a.Name, Email: a.Email},
			PartStat: neutralToPartStat(a.Status),
		})
	}

	return &scheduling.Invite{
		Method:    scheduling.MethodRequest,
		UID:       ev.UID,
		Summary:   ev.Subject,
		Start:     ev.Start,
		End:       ev.End,
		Sequence:  ev.Sequence,
		Organizer: toSchedulingAddress(ev.Organizer),
		Attendees: attendees,
	}
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
func ProcessRequest(ctx context.Context, w calendar.Writer, calendarID string, raw []byte) (calendar.Event, error) {
	inv, err := scheduling.Parse(raw)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("itip: parse request: %w", err)
	}
	if inv.Method != scheduling.MethodRequest {
		return calendar.Event{}, fmt.Errorf("%w (method %q)", ErrNotRequest, inv.Method)
	}

	event := EventFromInvite(inv)

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
			if inv.Sequence <= existing.Sequence {
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
// It shares [Respond]'s RECURRENCE-ID first-cut limitation via [InviteFromEvent]:
// the reply addresses the master event (no per-instance RECURRENCE-ID), at the
// stored event's SEQUENCE.
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

// toCalendarAddress maps a scheduling.Address onto the calendar port's Address.
// The two types are structurally identical (display name + email) but live in
// separate packages, so the bridge is explicit.
func toCalendarAddress(a scheduling.Address) calendar.Address {
	return calendar.Address{Name: a.Name, Email: a.Email}
}

// toSchedulingAddress maps a calendar.Address onto the scheduling engine's
// Address — the reverse of toCalendarAddress. The two types are structurally
// identical (display name + email) but live in separate packages, so the bridge
// is explicit.
func toSchedulingAddress(a calendar.Address) scheduling.Address {
	return scheduling.Address{Name: a.Name, Email: a.Email}
}

// partStatToNeutral maps an iCalendar PARTSTAT to the neutral
// calendar.Attendee.Status (Graph responseStatus shape); neutralToPartStat is the
// inverse. An unmapped value yields the zero value.
func partStatToNeutral(p scheduling.PartStat) string {
	switch p {
	case scheduling.PartStatAccepted:
		return "accepted"
	case scheduling.PartStatDeclined:
		return "declined"
	case scheduling.PartStatTentative:
		return "tentativelyAccepted"
	case scheduling.PartStatNeedsAction:
		return "notResponded"
	default:
		return ""
	}
}

func neutralToPartStat(s string) scheduling.PartStat {
	switch s {
	case "accepted":
		return scheduling.PartStatAccepted
	case "declined":
		return scheduling.PartStatDeclined
	case "tentativelyAccepted":
		return scheduling.PartStatTentative
	case "notResponded":
		return scheduling.PartStatNeedsAction
	default:
		return ""
	}
}

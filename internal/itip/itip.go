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

	attendees := make([]calendar.Address, 0, len(inv.Attendees))
	for _, a := range inv.Attendees {
		attendees = append(attendees, toCalendarAddress(a.Address))
	}

	return calendar.Event{
		UID:       inv.UID,
		Subject:   inv.Summary,
		Start:     inv.Start,
		End:       inv.End,
		Organizer: toCalendarAddress(inv.Organizer),
		Attendees: attendees,
		Status:    StatusTentative,
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
// DEFERRED: UID-correlated update of an existing tentative event. A re-sent
// REQUEST (same UID, higher SEQUENCE) should locate and update the event the
// first REQUEST created rather than creating a duplicate; that requires a UID
// lookup on the backend and is part of the trigger follow-up. Today this always
// creates.
func ProcessRequest(ctx context.Context, w calendar.Writer, calendarID string, raw []byte) (calendar.Event, error) {
	inv, err := scheduling.Parse(raw)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("itip: parse request: %w", err)
	}
	if inv.Method != scheduling.MethodRequest {
		return calendar.Event{}, fmt.Errorf("%w (method %q)", ErrNotRequest, inv.Method)
	}

	event := EventFromInvite(inv)
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

// toCalendarAddress maps a scheduling.Address onto the calendar port's Address.
// The two types are structurally identical (display name + email) but live in
// separate packages, so the bridge is explicit.
func toCalendarAddress(a scheduling.Address) calendar.Address {
	return calendar.Address{Name: a.Name, Email: a.Email}
}

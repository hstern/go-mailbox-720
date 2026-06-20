// Package scheduling implements the client-side core of the iTIP/iMIP
// calendar-scheduling engine for backends that do not do scheduling themselves.
//
// Exchange's invite cooperation is iTIP (RFC 5546) — a set of scheduling
// "methods" (REQUEST to invite, REPLY to respond, CANCEL to withdraw) carried
// between an organizer and attendees. When those methods travel over email the
// transport is iMIP (RFC 6047): an ordinary RFC 822 message carrying a
// "text/calendar" body part whose "method" parameter names the iTIP method and
// whose content is a VCALENDAR holding the scheduling object.
//
// A CalDAV client talking to a non-scheduling server (the "dumb backend" case)
// must play the scheduling role itself: detect an inbound REQUEST and surface it
// to the user as a tentative event, and on the user's accept/decline send a
// METHOD:REPLY iMIP back to the organizer. Everything correlates by the
// iCalendar UID (plus RECURRENCE-ID for a single instance of a series).
//
// This package is the FIRST CUT: the parse + reply-generation core only. It
// turns a raw iMIP message into an [Invite] ([Parse]) and builds the REPLY
// VCALENDAR body for an accept/decline/tentative ([Reply]). Wiring this into the
// mail/calendar handlers, the backend-capability switch (delegate to RFC 6638
// servers vs. run this engine), organizer-side response tracking, and the SMTP
// send of the reply are deferred to follow-up issues (MB720-10 and friends).
package scheduling

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// ErrNoCalendar is returned by Parse when the message carries no text/calendar
// part — i.e. it is ordinary mail, not an iMIP scheduling message. A caller
// scanning a mailbox uses errors.Is to skip non-invites without treating them as
// failures.
var ErrNoCalendar = errors.New("scheduling: no text/calendar part in message")

// Method is an iTIP scheduling method (RFC 5546), carried as the "method"
// parameter of an iMIP "text/calendar" part and as the VCALENDAR METHOD
// property. Only the subset this engine handles is modelled.
type Method string

const (
	// MethodRequest invites attendees to an event (organizer -> attendee).
	MethodRequest Method = "REQUEST"
	// MethodReply responds to a REQUEST (attendee -> organizer).
	MethodReply Method = "REPLY"
	// MethodCancel withdraws a previously requested event (organizer -> attendee).
	MethodCancel Method = "CANCEL"
)

// PartStat is an attendee's participation status (the iCalendar PARTSTAT
// parameter), set on the ATTENDEE of a REPLY to convey the user's decision.
type PartStat string

const (
	// PartStatAccepted means the attendee accepted the invitation.
	PartStatAccepted PartStat = "ACCEPTED"
	// PartStatDeclined means the attendee declined the invitation.
	PartStatDeclined PartStat = "DECLINED"
	// PartStatTentative means the attendee tentatively accepted the invitation.
	PartStatTentative PartStat = "TENTATIVE"
	// PartStatNeedsAction means the attendee has not yet responded; it is the
	// status set on every ATTENDEE of an outbound REQUEST.
	PartStatNeedsAction PartStat = "NEEDS-ACTION"
)

// Status is an event's overall status (the iCalendar STATUS property). Only the
// CANCELLED value, set on the VEVENT of an outbound CANCEL, is modelled.
type Status string

const (
	// StatusCancelled marks an event as withdrawn; a METHOD:CANCEL VEVENT carries
	// it per RFC 5546 §3.2.5.
	StatusCancelled Status = "CANCELLED"
)

// ScheduleStatus is an RFC 6638 SCHEDULE-STATUS value: an iTIP REQUEST-STATUS code
// (RFC 5546 §3.6) recording the delivery outcome of a scheduling message, which a
// server stamps on each ATTENDEE it tried to notify. Only the two outcomes the
// engine produces for an iMIP REQUEST are modelled; the iMIP transport is
// fire-and-forget after SMTP submission, so success is "sent", not "delivered".
type ScheduleStatus string

const (
	// SchedStatusSent ("1.1") means the scheduling message was successfully
	// submitted to the SMTP relay. Final delivery to the attendee is unconfirmed.
	SchedStatusSent ScheduleStatus = "1.1"
	// SchedStatusNoDelivery ("5.1") means the scheduling message could not be
	// delivered (SMTP submission failed). RFC 5546 also defines 3.7 for an invalid
	// calendar user; distinguishing it needs the SMTP reply code, a later refinement.
	SchedStatusNoDelivery ScheduleStatus = "5.1"
)

// Address is a calendar-user address: a display name plus an email. It mirrors
// calendar.Address; iCalendar carries these as CAL-ADDRESS values (a "mailto:"
// URI with an optional CN parameter for the display name).
type Address struct {
	Name  string
	Email string
}

// Attendee is one ATTENDEE of a scheduling object: the calendar-user address
// plus that attendee's participation status.
type Attendee struct {
	Address
	PartStat PartStat
}

// Invite is a parsed iTIP scheduling object — the first VEVENT of the VCALENDAR
// carried by an iMIP message, together with the iTIP method. Correlation across
// the invite's lifetime is by UID (and RecurrenceID for a single instance of a
// recurring series).
type Invite struct {
	Method       Method
	UID          string
	RecurrenceID string // RECURRENCE-ID of a single overridden instance, "" for the master
	Organizer    Address
	Attendees    []Attendee
	Summary      string
	Start        time.Time
	End          time.Time
	Sequence     int

	// recurrenceProp preserves the original RECURRENCE-ID property (its value
	// type, TZID, and RANGE parameters) so Reply can echo it faithfully rather
	// than guessing its DATE/DATE-TIME shape from the RecurrenceID string. Nil
	// for a master (non-instance) invite.
	recurrenceProp *ical.Prop
}

// productID identifies this engine in generated VCALENDAR objects (the PRODID
// property required by RFC 5545).
const productID = "-//go-mailbox-720//iTIP scheduling//EN"

// Parse reads a raw RFC 822 iMIP message, finds its "text/calendar" part, parses
// the VCALENDAR, and maps the METHOD plus the first VEVENT into an Invite. It
// returns an error if the message cannot be read or carries no calendar part.
func Parse(raw []byte) (*Invite, error) {
	// CreateReader returns a usable reader alongside an IsUnknownCharset error
	// when the message declares a charset Go cannot decode; iCalendar bodies are
	// ASCII-safe, so press on in that case rather than failing the whole parse.
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return nil, fmt.Errorf("scheduling: read message: %w", err)
	}
	defer func() { _ = mr.Close() }()

	cal, err := findCalendar(mr)
	if err != nil {
		return nil, err
	}

	return inviteFromCalendar(cal)
}

// findCalendar walks the mail parts and decodes the first "text/calendar" part
// as a VCALENDAR. It returns an error if no such part exists.
func findCalendar(mr *mail.Reader) (*ical.Calendar, error) {
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil, ErrNoCalendar
		}
		// An IsUnknownCharset error still yields a usable part; only a genuine
		// read error aborts the walk.
		if err != nil && !message.IsUnknownCharset(err) {
			return nil, fmt.Errorf("scheduling: read part: %w", err)
		}

		mediaType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || !strings.EqualFold(mediaType, ical.MIMEType) {
			continue
		}

		cal, err := ical.NewDecoder(part.Body).Decode()
		if err != nil {
			return nil, fmt.Errorf("scheduling: decode calendar part: %w", err)
		}
		return cal, nil
	}
}

// inviteFromCalendar maps a decoded VCALENDAR (its METHOD and first VEVENT) onto
// an Invite. It returns an error if the calendar carries no VEVENT.
func inviteFromCalendar(cal *ical.Calendar) (*Invite, error) {
	events := cal.Events()
	if len(events) == 0 {
		return nil, fmt.Errorf("scheduling: calendar has no VEVENT")
	}
	ev := &events[0]

	inv := &Invite{
		Method:    Method(strings.ToUpper(propText(cal.Props, ical.PropMethod))),
		UID:       propText(ev.Props, ical.PropUID),
		Summary:   propText(ev.Props, ical.PropSummary),
		Organizer: parseAddress(ev.Props.Get(ical.PropOrganizer)),
		Sequence:  propInt(ev.Props, ical.PropSequence),
	}
	// RECURRENCE-ID is a DATE-TIME (or DATE) value, not text: keep its raw value
	// for correlation and a copy of the whole property so Reply can echo its
	// value type and TZID/RANGE parameters faithfully.
	if rid := ev.Props.Get(ical.PropRecurrenceID); rid != nil {
		inv.RecurrenceID = rid.Value
		ridCopy := *rid
		ridCopy.Params = cloneParams(rid.Params)
		inv.recurrenceProp = &ridCopy
	}
	if start, err := ev.DateTimeStart(time.UTC); err == nil {
		inv.Start = start
	}
	if end, err := ev.DateTimeEnd(time.UTC); err == nil {
		inv.End = end
	}
	for _, p := range ev.Props.Values(ical.PropAttendee) {
		addr := parseAddress(&p)
		if addr == (Address{}) {
			continue
		}
		inv.Attendees = append(inv.Attendees, Attendee{
			Address:  addr,
			PartStat: PartStat(strings.ToUpper(p.Params.Get(ical.ParamParticipationStatus))),
		})
	}
	return inv, nil
}

// Reply builds a METHOD:REPLY VCALENDAR responding to a parsed REQUEST and
// returns it serialized as iCalendar bytes (CRLF line endings), ready to be
// emailed back to the organizer as the "text/calendar; method=REPLY" body.
//
// The reply echoes the request's UID, SEQUENCE, RECURRENCE-ID, ORGANIZER, and
// SUMMARY, and carries a single ATTENDEE — the responding user — with PARTSTAT
// set to the user's decision. Per RFC 5546 a REPLY conveys exactly the
// responding attendee's status, not the full attendee roster.
func Reply(req *Invite, attendee Address, partStat PartStat) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("scheduling: nil request")
	}
	if req.UID == "" {
		return nil, fmt.Errorf("scheduling: request has no UID")
	}

	cal, ev := newScheduling(MethodReply, req.UID, req.Sequence)
	// RFC 5546 §3.2.3 requires DTSTART in a REPLY VEVENT.
	if !req.Start.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeStart, req.Start)
	}
	if req.Summary != "" {
		ev.Props.SetText(ical.PropSummary, req.Summary)
	}
	echoRecurrenceID(ev, req)
	if org := buildAddress(ical.PropOrganizer, req.Organizer, ""); org != nil {
		ev.Props.Set(org)
	}
	if att := buildAddress(ical.PropAttendee, attendee, partStat); att != nil {
		ev.Props.Set(att)
	}
	cal.Children = append(cal.Children, ev.Component)

	return encodeCalendar(cal, "reply")
}

// Request builds a METHOD:REQUEST VCALENDAR/VEVENT — the organizer's invitation,
// mailed to attendees when an event with attendees is created or updated
// (RFC 5546 §3.2.2). It carries the event's UID, SEQUENCE, DTSTART/DTEND,
// SUMMARY, the ORGANIZER, and every ATTENDEE marked PARTSTAT=NEEDS-ACTION with
// RSVP=TRUE so each recipient is prompted to respond. The result is serialized
// as iCalendar bytes (CRLF line endings) for the "text/calendar; method=REQUEST"
// body.
//
// It returns an error if the invite has no UID, no ORGANIZER, or no ATTENDEE: a
// REQUEST without an organizer and at least one attendee is not a meaningful
// scheduling object.
func Request(inv Invite) ([]byte, error) {
	if inv.UID == "" {
		return nil, fmt.Errorf("scheduling: request has no UID")
	}
	if inv.Organizer.Email == "" {
		return nil, fmt.Errorf("scheduling: request has no organizer")
	}
	if len(inv.Attendees) == 0 {
		return nil, fmt.Errorf("scheduling: request has no attendees")
	}

	cal, ev := newScheduling(MethodRequest, inv.UID, inv.Sequence)
	// RFC 5546 §3.2.2 requires DTSTART; DTEND/SUMMARY round out the event.
	if !inv.Start.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeStart, inv.Start)
	}
	if !inv.End.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeEnd, inv.End)
	}
	if inv.Summary != "" {
		ev.Props.SetText(ical.PropSummary, inv.Summary)
	}
	echoRecurrenceID(ev, &inv)
	if org := buildAddress(ical.PropOrganizer, inv.Organizer, ""); org != nil {
		ev.Props.Set(org)
	}
	// Each ATTENDEE is invited fresh: NEEDS-ACTION participation status and
	// RSVP=TRUE ask the recipient to reply.
	for _, att := range inv.Attendees {
		prop := buildAddress(ical.PropAttendee, att.Address, PartStatNeedsAction)
		if prop == nil {
			continue
		}
		prop.Params.Set(ical.ParamRSVP, "TRUE")
		ev.Props.Add(prop)
	}
	cal.Children = append(cal.Children, ev.Component)

	return encodeCalendar(cal, "request")
}

// Cancel builds a METHOD:CANCEL VCALENDAR/VEVENT withdrawing a previously
// requested event, mailed to attendees when the organizer deletes it
// (RFC 5546 §3.2.5). It echoes the event's UID, carries STATUS:CANCELLED, the
// ORGANIZER, and every ATTENDEE, and bumps SEQUENCE — using the invite's
// Sequence when set, otherwise one past the requested value — so recipients see
// it supersedes the original REQUEST. The result is serialized as iCalendar
// bytes (CRLF line endings) for the "text/calendar; method=CANCEL" body.
//
// It returns an error if the invite has no UID or no ORGANIZER.
func Cancel(inv Invite) ([]byte, error) {
	if inv.UID == "" {
		return nil, fmt.Errorf("scheduling: cancel has no UID")
	}
	if inv.Organizer.Email == "" {
		return nil, fmt.Errorf("scheduling: cancel has no organizer")
	}

	cal, ev := newScheduling(MethodCancel, inv.UID, inv.Sequence)
	// A CANCEL must be marked STATUS:CANCELLED (RFC 5546 §3.2.5).
	ev.Props.SetText(ical.PropStatus, string(StatusCancelled))
	if !inv.Start.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeStart, inv.Start)
	}
	if inv.Summary != "" {
		ev.Props.SetText(ical.PropSummary, inv.Summary)
	}
	echoRecurrenceID(ev, &inv)
	if org := buildAddress(ical.PropOrganizer, inv.Organizer, ""); org != nil {
		ev.Props.Set(org)
	}
	for _, att := range inv.Attendees {
		if prop := buildAddress(ical.PropAttendee, att.Address, ""); prop != nil {
			ev.Props.Add(prop)
		}
	}
	cal.Children = append(cal.Children, ev.Component)

	return encodeCalendar(cal, "cancel")
}

// newScheduling builds a fresh VCALENDAR carrying the given iTIP method plus an
// empty VEVENT seeded with the shared properties every scheduling object needs:
// UID, a current DTSTAMP, and SEQUENCE. Callers add the method-specific
// properties (times, SUMMARY, STATUS, ORGANIZER/ATTENDEE) and append the event
// to the calendar's children.
func newScheduling(method Method, uid string, sequence int) (*ical.Calendar, *ical.Event) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, productID)
	cal.Props.SetText(ical.PropMethod, string(method))

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	setInt(ev.Props, ical.PropSequence, sequence)
	return cal, ev
}

// echoRecurrenceID copies a preserved RECURRENCE-ID property from inv onto ev so
// its DATE-TIME value type and TZID/RANGE parameters survive — SetText would
// force an incorrect VALUE=TEXT. It is a no-op for a master (non-instance)
// invite.
func echoRecurrenceID(ev *ical.Event, inv *Invite) {
	if inv.recurrenceProp == nil {
		return
	}
	rid := *inv.recurrenceProp
	rid.Params = cloneParams(inv.recurrenceProp.Params)
	ev.Props.Set(&rid)
}

// encodeCalendar serializes a VCALENDAR to iCalendar bytes (CRLF line endings),
// wrapping any encode error with the scheduling method's name for context.
func encodeCalendar(cal *ical.Calendar, what string) ([]byte, error) {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("scheduling: encode %s: %w", what, err)
	}
	return buf.Bytes(), nil
}

// parseAddress maps an iCalendar CAL-ADDRESS property (ORGANIZER / ATTENDEE) to
// an Address. The value is a "mailto:" URI; the optional CN parameter carries
// the display name. Returns the zero Address when prop is nil.
func parseAddress(prop *ical.Prop) Address {
	if prop == nil {
		return Address{}
	}
	email := strings.TrimSpace(prop.Value)
	if i := strings.IndexByte(email, ':'); i >= 0 && strings.EqualFold(email[:i], "mailto") {
		email = email[i+1:]
	}
	return Address{
		Name:  prop.Params.Get(ical.ParamCommonName),
		Email: email,
	}
}

// buildAddress constructs a CAL-ADDRESS property (ORGANIZER or ATTENDEE) from an
// Address, encoding the email as a "mailto:" URI and the name as a CN parameter.
// When partStat is non-empty it is added as the PARTSTAT parameter. Returns nil
// when the address has no email.
func buildAddress(name string, addr Address, partStat PartStat) *ical.Prop {
	if addr.Email == "" {
		return nil
	}
	prop := ical.NewProp(name)
	prop.Value = "mailto:" + addr.Email
	if addr.Name != "" {
		// SECURITY: sanitize the caller-supplied display name. go-ical escapes
		// TEXT property *values* but not parameter values, so a CN containing
		// CR/LF would otherwise inject forged property lines into the encoded
		// object (the same bug fixed in the CalDAV write path).
		prop.Params.Set(ical.ParamCommonName, sanitizeParam(addr.Name))
	}
	if partStat != "" {
		prop.Params.Set(ical.ParamParticipationStatus, string(partStat))
	}
	return prop
}

// sanitizeParam strips characters unsafe in an iCalendar property parameter
// value. go-ical escapes TEXT property values but not parameter values, so
// without this a CN (display name) containing CR/LF could inject forged property
// lines into the encoded object, and a double quote could break out of a quoted
// parameter value.
func sanitizeParam(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' {
			return -1
		}
		return r
	}, s)
}

// cloneParams deep-copies an iCalendar parameter set so a copied property does
// not alias the original's parameter slices. Returns nil for a nil input.
func cloneParams(src ical.Params) ical.Params {
	if src == nil {
		return nil
	}
	dst := make(ical.Params, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

// propText returns a property's text value, or "" if absent or unreadable.
func propText(props ical.Props, name string) string {
	if v, err := props.Text(name); err == nil {
		return v
	}
	return ""
}

// propInt returns a property's integer value, or 0 if absent or unreadable.
func propInt(props ical.Props, name string) int {
	if p := props.Get(name); p != nil {
		if v, err := p.Int(); err == nil {
			return v
		}
	}
	return 0
}

// setInt sets a property to the decimal string form of an integer.
func setInt(props ical.Props, name string, v int) {
	props.SetText(name, fmt.Sprintf("%d", v))
}

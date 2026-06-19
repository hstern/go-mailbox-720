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
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

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
			return nil, fmt.Errorf("scheduling: no text/calendar part in message")
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

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, productID)
	cal.Props.SetText(ical.PropMethod, string(MethodReply))

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, req.UID)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	setInt(ev.Props, ical.PropSequence, req.Sequence)
	// RFC 5546 §3.2.3 requires DTSTART in a REPLY VEVENT.
	if !req.Start.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeStart, req.Start)
	}
	if req.Summary != "" {
		ev.Props.SetText(ical.PropSummary, req.Summary)
	}
	// Echo RECURRENCE-ID from the preserved property so its DATE-TIME value type
	// and TZID/RANGE parameters survive — SetText would force an incorrect
	// VALUE=TEXT.
	if req.recurrenceProp != nil {
		rid := *req.recurrenceProp
		rid.Params = cloneParams(req.recurrenceProp.Params)
		ev.Props.Set(&rid)
	}
	if org := buildAddress(ical.PropOrganizer, req.Organizer, ""); org != nil {
		ev.Props.Set(org)
	}
	if att := buildAddress(ical.PropAttendee, attendee, partStat); att != nil {
		ev.Props.Set(att)
	}
	cal.Children = append(cal.Children, ev.Component)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("scheduling: encode reply: %w", err)
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
		prop.Params.Set(ical.ParamCommonName, addr.Name)
	}
	if partStat != "" {
		prop.Params.Set(ical.ParamParticipationStatus, string(partStat))
	}
	return prop
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

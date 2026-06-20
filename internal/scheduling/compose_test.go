package scheduling

import (
	"mime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

// fixedDate is a deterministic timestamp for compose tests — Compose takes the
// date as a parameter precisely so output is reproducible (no time.Now).
var fixedDate = time.Date(2026, 6, 19, 9, 30, 0, 0, time.UTC)

// TestComposeReplyRoundTrip composes a REPLY iMIP message and feeds it back
// through Parse, proving the text/calendar part and its method parameter are
// well-formed: the recovered Invite must carry Method=REPLY plus the original
// UID, organizer, and attendee.
func TestComposeReplyRoundTrip(t *testing.T) {
	req, err := Parse(requestMessage(t))
	if err != nil {
		t.Fatalf("parse request fixture: %v", err)
	}

	attendee := Address{Name: "Andy Attendee", Email: "andy@example.com"}
	ics, err := Reply(req, attendee, PartStatAccepted)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	raw, err := Compose(attendee, []Address{req.Organizer}, "Accepted: Project sync", MethodReply, ics, fixedDate)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	// Round-trip: the composed message must parse back to a REPLY invite.
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(composed): %v", err)
	}
	if got.Method != MethodReply {
		t.Errorf("Method = %q, want %q", got.Method, MethodReply)
	}
	if got.UID != req.UID {
		t.Errorf("UID = %q, want %q", got.UID, req.UID)
	}
	if got.Organizer.Email != req.Organizer.Email {
		t.Errorf("Organizer = %q, want %q", got.Organizer.Email, req.Organizer.Email)
	}
	if len(got.Attendees) != 1 {
		t.Fatalf("got %d attendees, want 1", len(got.Attendees))
	}
	if got.Attendees[0].Email != attendee.Email {
		t.Errorf("attendee = %q, want %q", got.Attendees[0].Email, attendee.Email)
	}
	if got.Attendees[0].PartStat != PartStatAccepted {
		t.Errorf("PARTSTAT = %q, want %q", got.Attendees[0].PartStat, PartStatAccepted)
	}
}

// TestComposeHeadersAndMethodParam asserts the message-level headers are present
// and that the calendar part carries the method Content-Type parameter equal to
// the iTIP METHOD with a UTF-8 charset.
func TestComposeHeadersAndMethodParam(t *testing.T) {
	from := Address{Name: "Andy Attendee", Email: "andy@example.com"}
	to := []Address{{Name: "Olivia Organizer", Email: "olivia@example.com"}}
	ics := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:abc-123@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	raw, err := Compose(from, to, "Accepted: Project sync", MethodReply, ics, fixedDate)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("CreateReader: %v", err)
	}
	t.Cleanup(func() { _ = mr.Close() })

	// Message-level headers.
	if got, _ := mr.Header.Subject(); got != "Accepted: Project sync" {
		t.Errorf("Subject = %q, want %q", got, "Accepted: Project sync")
	}
	if addrs, err := mr.Header.AddressList("From"); err != nil || len(addrs) != 1 || addrs[0].Address != from.Email {
		t.Errorf("From = %v (err %v), want %q", addrs, err, from.Email)
	}
	if addrs, err := mr.Header.AddressList("To"); err != nil || len(addrs) != 1 || addrs[0].Address != to[0].Email {
		t.Errorf("To = %v (err %v), want %q", addrs, err, to[0].Email)
	}
	if d, err := mr.Header.Date(); err != nil || !d.Equal(fixedDate) {
		t.Errorf("Date = %v (err %v), want %v", d, err, fixedDate)
	}
	if mr.Header.Get("MIME-Version") == "" {
		t.Error("MIME-Version header missing")
	}
	if mr.Header.Get("Message-ID") == "" {
		t.Error("Message-ID header missing")
	}

	// The calendar part must declare text/calendar with method=REPLY; charset.
	var sawCalendar, sawPlain bool
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		mediaType, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("ParseMediaType: %v", err)
		}
		switch mediaType {
		case "text/plain":
			sawPlain = true
		case "text/calendar":
			sawCalendar = true
			if !strings.EqualFold(params["method"], string(MethodReply)) {
				t.Errorf("calendar method param = %q, want %q", params["method"], MethodReply)
			}
			if !strings.EqualFold(params["charset"], "UTF-8") {
				t.Errorf("calendar charset param = %q, want UTF-8", params["charset"])
			}
		}
	}
	if !sawCalendar {
		t.Error("no text/calendar part in composed message")
	}
	if !sawPlain {
		t.Error("no text/plain alternative part in composed message")
	}
}

// TestComposeRequestRoundTrip composes a REQUEST message from a Request-built
// VCALENDAR and round-trips it through Parse, recovering Method=REQUEST, the
// UID, organizer, and attendees.
func TestComposeRequestRoundTrip(t *testing.T) {
	organizer := Address{Name: "Olivia Organizer", Email: "olivia@example.com"}
	attendee := Address{Name: "Andy Attendee", Email: "andy@example.com"}
	inv := Invite{
		UID:       "req-456@example.com",
		Organizer: organizer,
		Summary:   "Quarterly planning",
		Start:     time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC),
		Attendees: []Attendee{{Address: attendee, PartStat: PartStatNeedsAction}},
	}
	ics, err := Request(inv)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	raw, err := Compose(organizer, []Address{attendee}, "Invitation: Quarterly planning", MethodRequest, ics, fixedDate)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(composed): %v", err)
	}
	if got.Method != MethodRequest {
		t.Errorf("Method = %q, want %q", got.Method, MethodRequest)
	}
	if got.UID != inv.UID {
		t.Errorf("UID = %q, want %q", got.UID, inv.UID)
	}
	if got.Organizer.Email != organizer.Email {
		t.Errorf("Organizer = %q, want %q", got.Organizer.Email, organizer.Email)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].Email != attendee.Email {
		t.Errorf("Attendees = %v, want one %q", got.Attendees, attendee.Email)
	}
}

// TestComposeReplyConvenience exercises the ComposeReply one-call path and
// asserts it produces a REPLY that parses back with the attendee's accepted
// status and an "Accepted: ..." subject addressed to the organizer.
func TestComposeReplyConvenience(t *testing.T) {
	req, err := Parse(requestMessage(t))
	if err != nil {
		t.Fatalf("parse request fixture: %v", err)
	}
	attendee := Address{Name: "Andy Attendee", Email: "andy@example.com"}

	raw, err := ComposeReply(req, attendee, PartStatAccepted, fixedDate)
	if err != nil {
		t.Fatalf("ComposeReply: %v", err)
	}

	mr, err := mail.CreateReader(strings.NewReader(string(raw)))
	if err != nil && !message.IsUnknownCharset(err) {
		t.Fatalf("CreateReader: %v", err)
	}
	t.Cleanup(func() { _ = mr.Close() })
	if got, _ := mr.Header.Subject(); got != "Accepted: Project sync" {
		t.Errorf("Subject = %q, want %q", got, "Accepted: Project sync")
	}
	if addrs, err := mr.Header.AddressList("To"); err != nil || len(addrs) != 1 || addrs[0].Address != req.Organizer.Email {
		t.Errorf("To = %v (err %v), want organizer %q", addrs, err, req.Organizer.Email)
	}

	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(composed): %v", err)
	}
	if got.Method != MethodReply {
		t.Errorf("Method = %q, want %q", got.Method, MethodReply)
	}
	if len(got.Attendees) != 1 || got.Attendees[0].PartStat != PartStatAccepted {
		t.Errorf("Attendees = %v, want one ACCEPTED", got.Attendees)
	}
}

// TestComposeDeterministicMessageID verifies the Message-ID is derived
// deterministically from the ics UID and sender domain — no clock, no
// randomness leaks into it. (The multipart boundary is intentionally random,
// per go-message, so the full bytes are not compared.)
func TestComposeDeterministicMessageID(t *testing.T) {
	from := Address{Email: "andy@example.com"}
	to := []Address{{Email: "olivia@example.com"}}
	ics := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:abc-123@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	id := func() string {
		raw, err := Compose(from, to, "Accepted", MethodReply, ics, fixedDate)
		if err != nil {
			t.Fatalf("Compose: %v", err)
		}
		mr, err := mail.CreateReader(strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("CreateReader: %v", err)
		}
		defer func() { _ = mr.Close() }()
		return mr.Header.Get("Message-ID")
	}

	a, b := id(), id()
	if a == "" {
		t.Fatal("Message-ID is empty")
	}
	if a != b {
		t.Errorf("Message-ID not deterministic: %q vs %q", a, b)
	}
	// Derived from the UID local part and the sender's domain.
	if want := "<abc-123@example.com>"; a != want {
		t.Errorf("Message-ID = %q, want %q", a, want)
	}
}

// TestComposeErrors covers the guard rails: a missing sender or no recipients.
func TestComposeErrors(t *testing.T) {
	ics := []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	if _, err := Compose(Address{}, []Address{{Email: "x@example.com"}}, "s", MethodReply, ics, fixedDate); err == nil {
		t.Error("Compose with no from address: want error, got nil")
	}
	if _, err := Compose(Address{Email: "x@example.com"}, nil, "s", MethodReply, ics, fixedDate); err == nil {
		t.Error("Compose with no recipients: want error, got nil")
	}
	if _, err := ComposeReply(nil, Address{Email: "x@example.com"}, PartStatAccepted, fixedDate); err == nil {
		t.Error("ComposeReply(nil): want error, got nil")
	}
}

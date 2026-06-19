package scheduling

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// requestMessage builds a raw multipart iMIP REQUEST: a text/plain part plus a
// text/calendar; method=REQUEST part holding a VCALENDAR/VEVENT. The calendar is
// constructed inline so the test owns the exact bytes.
func requestMessage(t *testing.T) []byte {
	t.Helper()
	ics := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//Example Corp//Calendar//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:abc-123@example.com\r\n" +
		"DTSTAMP:20260601T120000Z\r\n" +
		"DTSTART:20260615T150000Z\r\n" +
		"DTEND:20260615T160000Z\r\n" +
		"SEQUENCE:2\r\n" +
		"SUMMARY:Project sync\r\n" +
		"ORGANIZER;CN=Olivia Organizer:mailto:olivia@example.com\r\n" +
		"ATTENDEE;CN=Andy Attendee;PARTSTAT=NEEDS-ACTION:mailto:andy@example.com\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	return mimeMessage(t, "REQUEST", ics)
}

// mimeMessage wraps an iCalendar body in a multipart/mixed RFC 822 message with
// the given iTIP method on the text/calendar part.
func mimeMessage(t *testing.T, method, ics string) []byte {
	t.Helper()
	const boundary = "boundary42"
	var b bytes.Buffer
	b.WriteString("From: olivia@example.com\r\n")
	b.WriteString("To: andy@example.com\r\n")
	b.WriteString("Subject: Invitation: Project sync\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("You are invited.\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/calendar; method=" + method + "; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(ics)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes()
}

func TestParse(t *testing.T) {
	wantStart := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 6, 15, 16, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		raw    []byte
		method Method
	}{
		{
			name:   "request",
			raw:    requestMessage(t),
			method: MethodRequest,
		},
		{
			name: "cancel",
			raw: mimeMessage(t, "CANCEL", "BEGIN:VCALENDAR\r\n"+
				"VERSION:2.0\r\n"+
				"PRODID:-//Example Corp//Calendar//EN\r\n"+
				"METHOD:CANCEL\r\n"+
				"BEGIN:VEVENT\r\n"+
				"UID:abc-123@example.com\r\n"+
				"DTSTAMP:20260602T120000Z\r\n"+
				"SEQUENCE:3\r\n"+
				"SUMMARY:Project sync\r\n"+
				"ORGANIZER;CN=Olivia Organizer:mailto:olivia@example.com\r\n"+
				"END:VEVENT\r\n"+
				"END:VCALENDAR\r\n"),
			method: MethodCancel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if inv.Method != tt.method {
				t.Errorf("Method = %q, want %q", inv.Method, tt.method)
			}
			if inv.UID != "abc-123@example.com" {
				t.Errorf("UID = %q, want %q", inv.UID, "abc-123@example.com")
			}
			if inv.Summary != "Project sync" {
				t.Errorf("Summary = %q, want %q", inv.Summary, "Project sync")
			}
			if inv.Organizer.Email != "olivia@example.com" {
				t.Errorf("Organizer.Email = %q, want %q", inv.Organizer.Email, "olivia@example.com")
			}
			if inv.Organizer.Name != "Olivia Organizer" {
				t.Errorf("Organizer.Name = %q, want %q", inv.Organizer.Name, "Olivia Organizer")
			}

			// REQUEST-only assertions: the cancel fixture carries no times/attendee.
			if tt.method == MethodRequest {
				if !inv.Start.Equal(wantStart) {
					t.Errorf("Start = %v, want %v", inv.Start, wantStart)
				}
				if !inv.End.Equal(wantEnd) {
					t.Errorf("End = %v, want %v", inv.End, wantEnd)
				}
				if inv.Sequence != 2 {
					t.Errorf("Sequence = %d, want 2", inv.Sequence)
				}
				if len(inv.Attendees) != 1 {
					t.Fatalf("Attendees len = %d, want 1", len(inv.Attendees))
				}
				att := inv.Attendees[0]
				if att.Email != "andy@example.com" {
					t.Errorf("Attendee.Email = %q, want %q", att.Email, "andy@example.com")
				}
				if att.Name != "Andy Attendee" {
					t.Errorf("Attendee.Name = %q, want %q", att.Name, "Andy Attendee")
				}
				if att.PartStat != "NEEDS-ACTION" {
					t.Errorf("Attendee.PartStat = %q, want %q", att.PartStat, "NEEDS-ACTION")
				}
			}
		})
	}
}

func TestParseNoCalendarPart(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: hi\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"just a plain message, no invite\r\n")

	if _, err := Parse(raw); err == nil {
		t.Fatal("Parse() error = nil, want error for message with no calendar part")
	}
}

func TestReplyRoundTrip(t *testing.T) {
	req, err := Parse(requestMessage(t))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	tests := []struct {
		name     string
		partStat PartStat
	}{
		{"accepted", PartStatAccepted},
		{"declined", PartStatDeclined},
		{"tentative", PartStatTentative},
	}

	responder := Address{Name: "Andy Attendee", Email: "andy@example.com"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Reply(req, responder, tt.partStat)
			if err != nil {
				t.Fatalf("Reply() error = %v", err)
			}
			if !bytes.Contains(out, []byte("\r\n")) {
				t.Error("Reply() output is not CRLF-delimited")
			}

			// Round-trip: the reply body parses back as a valid VCALENDAR.
			cal, err := ical.NewDecoder(bytes.NewReader(out)).Decode()
			if err != nil {
				t.Fatalf("decode reply: %v", err)
			}

			if m, _ := cal.Props.Text(ical.PropMethod); m != string(MethodReply) {
				t.Errorf("reply METHOD = %q, want %q", m, MethodReply)
			}

			events := cal.Events()
			if len(events) != 1 {
				t.Fatalf("reply events len = %d, want 1", len(events))
			}
			ev := &events[0]

			if uid, _ := ev.Props.Text(ical.PropUID); uid != req.UID {
				t.Errorf("reply UID = %q, want %q", uid, req.UID)
			}
			if seq := ev.Props.Get(ical.PropSequence); seq == nil || seq.Value != "2" {
				t.Errorf("reply SEQUENCE = %v, want 2", seq)
			}

			org := ev.Props.Get(ical.PropOrganizer)
			if org == nil || !strings.Contains(org.Value, "olivia@example.com") {
				t.Errorf("reply ORGANIZER = %v, want olivia@example.com", org)
			}

			att := ev.Props.Get(ical.PropAttendee)
			if att == nil {
				t.Fatal("reply has no ATTENDEE")
			}
			if !strings.Contains(att.Value, "andy@example.com") {
				t.Errorf("reply ATTENDEE = %q, want andy@example.com", att.Value)
			}
			if got := att.Params.Get(ical.ParamParticipationStatus); got != string(tt.partStat) {
				t.Errorf("reply PARTSTAT = %q, want %q", got, tt.partStat)
			}
		})
	}
}

func TestReplyRecurringInstance(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//Example Corp//Calendar//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:series-9@example.com\r\n" +
		"DTSTAMP:20260601T120000Z\r\n" +
		"DTSTART:20260615T150000Z\r\n" +
		"DTEND:20260615T160000Z\r\n" +
		"RECURRENCE-ID:20260615T150000Z\r\n" +
		"SEQUENCE:1\r\n" +
		"SUMMARY:Weekly sync\r\n" +
		"ORGANIZER;CN=Olivia:mailto:olivia@example.com\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:andy@example.com\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	req, err := Parse(mimeMessage(t, "REQUEST", ics))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if req.RecurrenceID != "20260615T150000Z" {
		t.Errorf("RecurrenceID = %q, want %q", req.RecurrenceID, "20260615T150000Z")
	}

	out, err := Reply(req, Address{Email: "andy@example.com"}, PartStatAccepted)
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}

	cal, err := ical.NewDecoder(bytes.NewReader(out)).Decode()
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	ev := &cal.Events()[0]

	// DTSTART is mandatory in a REPLY (RFC 5546 §3.2.3).
	if got, err := ev.DateTimeStart(time.UTC); err != nil {
		t.Errorf("reply DTSTART error = %v, want a value", err)
	} else if want := time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("reply DTSTART = %v, want %v", got, want)
	}

	// RECURRENCE-ID must round-trip as a DATE-TIME, not VALUE=TEXT.
	rid := ev.Props.Get(ical.PropRecurrenceID)
	if rid == nil {
		t.Fatal("reply has no RECURRENCE-ID")
	}
	if rid.Value != "20260615T150000Z" {
		t.Errorf("reply RECURRENCE-ID value = %q, want %q", rid.Value, "20260615T150000Z")
	}
	if vt := rid.Params.Get(ical.ParamValue); vt == "TEXT" {
		t.Errorf("reply RECURRENCE-ID has VALUE=TEXT, want a DATE-TIME value")
	}
}

func TestReplyErrors(t *testing.T) {
	if _, err := Reply(nil, Address{Email: "a@example.com"}, PartStatAccepted); err == nil {
		t.Error("Reply(nil) error = nil, want error")
	}
	if _, err := Reply(&Invite{}, Address{Email: "a@example.com"}, PartStatAccepted); err == nil {
		t.Error("Reply(no UID) error = nil, want error")
	}
}

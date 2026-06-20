package caldav

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// decodeCalendar parses a VCALENDAR fixture. iCalendar requires CRLF line
// endings, so the fixtures are written with \n and normalized here.
func decodeCalendar(t *testing.T, s string) *ical.Calendar {
	t.Helper()
	s = strings.ReplaceAll(s, "\n", "\r\n")
	cal, err := ical.NewDecoder(strings.NewReader(s)).Decode()
	if err != nil {
		t.Fatalf("decode calendar: %v", err)
	}
	return cal
}

const timedEvent = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:event-123@example.com
SUMMARY:Quarterly Planning
DESCRIPTION:Discuss roadmap and OKRs.
LOCATION:Conference Room B
STATUS:CONFIRMED
CREATED:20260601T090000Z
DTSTART:20260619T130000Z
DTEND:20260619T143000Z
ORGANIZER;CN=Alice Smith:mailto:alice@example.com
ATTENDEE;CN=Bob Jones:mailto:bob@example.com
ATTENDEE:mailto:carol@example.com
END:VEVENT
END:VCALENDAR
`

func TestMapEventTimed(t *testing.T) {
	cal := decodeCalendar(t, timedEvent)
	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := mapEvent(&events[0])

	if e.Subject != "Quarterly Planning" {
		t.Errorf("Subject = %q, want %q", e.Subject, "Quarterly Planning")
	}
	if e.UID != "event-123@example.com" {
		t.Errorf("UID = %q, want %q", e.UID, "event-123@example.com")
	}
	if e.Location != "Conference Room B" {
		t.Errorf("Location = %q, want %q", e.Location, "Conference Room B")
	}
	if e.Status != "confirmed" {
		t.Errorf("Status = %q, want %q", e.Status, "confirmed")
	}
	if e.Body.ContentType != "text" || e.Body.Content != "Discuss roadmap and OKRs." {
		t.Errorf("Body = %+v, want text/%q", e.Body, "Discuss roadmap and OKRs.")
	}
	if e.IsAllDay {
		t.Error("IsAllDay = true, want false for a timed event")
	}

	wantStart := time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", e.Start, wantStart)
	}
	wantEnd := time.Date(2026, 6, 19, 14, 30, 0, 0, time.UTC)
	if !e.End.Equal(wantEnd) {
		t.Errorf("End = %v, want %v", e.End, wantEnd)
	}
	wantCreated := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	if !e.CreatedAt.Equal(wantCreated) {
		t.Errorf("CreatedAt = %v, want %v", e.CreatedAt, wantCreated)
	}

	wantOrg := calendar.Address{Name: "Alice Smith", Email: "alice@example.com"}
	if e.Organizer != wantOrg {
		t.Errorf("Organizer = %+v, want %+v", e.Organizer, wantOrg)
	}
	if len(e.Attendees) != 2 {
		t.Fatalf("got %d attendees, want 2", len(e.Attendees))
	}
	if got := e.Attendees[0]; got != (calendar.Attendee{Name: "Bob Jones", Email: "bob@example.com"}) {
		t.Errorf("Attendees[0] = %+v", got)
	}
	if got := e.Attendees[1]; got != (calendar.Attendee{Name: "", Email: "carol@example.com"}) {
		t.Errorf("Attendees[1] = %+v", got)
	}
}

const allDayEvent = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:holiday-1@example.com
SUMMARY:Company Holiday
DTSTART;VALUE=DATE:20260620
DTEND;VALUE=DATE:20260621
END:VEVENT
END:VCALENDAR
`

func TestMapEventAllDay(t *testing.T) {
	cal := decodeCalendar(t, allDayEvent)
	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := mapEvent(&events[0])

	if !e.IsAllDay {
		t.Error("IsAllDay = false, want true for a VALUE=DATE event")
	}
	if e.Subject != "Company Holiday" {
		t.Errorf("Subject = %q", e.Subject)
	}
	wantStart := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", e.Start, wantStart)
	}
	// Absent optional fields map to zero values, not errors.
	if e.Location != "" || e.Status != "" || e.Body.Content != "" {
		t.Errorf("expected empty optional fields, got Location=%q Status=%q Body=%q", e.Location, e.Status, e.Body.Content)
	}
	if e.Organizer != (calendar.Address{Name: "", Email: ""}) {
		t.Errorf("Organizer = %+v, want zero", e.Organizer)
	}
	if len(e.Attendees) != 0 {
		t.Errorf("Attendees = %+v, want none", e.Attendees)
	}
}

func TestEventFromObjectSetsIDs(t *testing.T) {
	cal := decodeCalendar(t, timedEvent)
	objectPath := "/calendars/alice/work/event-123.ics"
	calID := calendarID("/calendars/alice/work/")

	e, ok := eventFromObject(calID, objectPath, cal)
	if !ok {
		t.Fatal("eventFromObject returned ok=false")
	}
	if e.ID != eventID(objectPath) {
		t.Errorf("ID = %q, want %q", e.ID, eventID(objectPath))
	}
	if e.CalendarID != calID {
		t.Errorf("CalendarID = %q, want %q", e.CalendarID, calID)
	}
}

func TestEventFromObjectNilCalendar(t *testing.T) {
	if _, ok := eventFromObject("cal", "/p.ics", nil); ok {
		t.Error("eventFromObject(nil) ok = true, want false")
	}
}

// eventToICal builds the VEVENT the write path PUTs; round-tripping it back
// through the read-path mapEvent must recover the event's fields, and the
// encoded form must carry the required structural properties.
func TestEventToICalRoundTrip(t *testing.T) {
	want := calendar.Event{
		UID:       "event-123@example.com",
		Subject:   "Quarterly Planning",
		Location:  "Conference Room B",
		Start:     time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 6, 19, 14, 30, 0, 0, time.UTC),
		Organizer: calendar.Address{Name: "Alice Smith", Email: "alice@example.com"},
		Attendees: []calendar.Attendee{
			{Name: "Bob Jones", Email: "bob@example.com"},
			{Email: "carol@example.com"},
		},
		Body: calendar.Body{ContentType: "text", Content: "Discuss roadmap and OKRs."},
	}

	cal := eventToICal(want)

	// The encoded object must be a single VEVENT carrying no METHOD (METHOD is
	// reserved for iTIP scheduling objects, not stored calendar resources) and
	// CRLF line endings per RFC 5545.
	var buf strings.Builder
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "METHOD:") {
		t.Errorf("encoded object carries a METHOD property:\n%s", out)
	}
	if !strings.Contains(out, "\r\n") {
		t.Error("encoded object is not CRLF-terminated")
	}
	if !strings.Contains(out, "DTSTAMP:") {
		t.Error("encoded object missing required DTSTAMP")
	}

	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := mapEvent(&events[0])

	if got.Subject != want.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, want.Subject)
	}
	if got.UID != want.UID {
		t.Errorf("UID = %q, want %q", got.UID, want.UID)
	}
	if got.Location != want.Location {
		t.Errorf("Location = %q, want %q", got.Location, want.Location)
	}
	if !got.Start.Equal(want.Start) {
		t.Errorf("Start = %v, want %v", got.Start, want.Start)
	}
	if !got.End.Equal(want.End) {
		t.Errorf("End = %v, want %v", got.End, want.End)
	}
	if got.Body.Content != want.Body.Content {
		t.Errorf("Body = %q, want %q", got.Body.Content, want.Body.Content)
	}
	if got.Organizer != want.Organizer {
		t.Errorf("Organizer = %+v, want %+v", got.Organizer, want.Organizer)
	}
	if len(got.Attendees) != len(want.Attendees) {
		t.Fatalf("got %d attendees, want %d", len(got.Attendees), len(want.Attendees))
	}
	for i := range want.Attendees {
		if got.Attendees[i] != want.Attendees[i] {
			t.Errorf("Attendees[%d] = %+v, want %+v", i, got.Attendees[i], want.Attendees[i])
		}
	}
}

// CreateEvent mints a UID when the input event has none; eventToICal must then
// still emit a UID (CalDAV resources require one).
func TestEventToICalMintedUID(t *testing.T) {
	cal := eventToICal(calendar.Event{UID: newUID(), Subject: "No UID supplied"})
	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if uid := propText(&events[0], ical.PropUID); uid == "" {
		t.Error("encoded event has empty UID")
	}
}

// A recurring series (override instance listed before the master) must be
// represented by its master VEVENT, not whichever component appears first.
func TestEventFromObjectPicksMaster(t *testing.T) {
	const recurring = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//test//EN
BEGIN:VEVENT
UID:series-1
RECURRENCE-ID:20250612T120000Z
DTSTART:20250612T130000Z
DTEND:20250612T140000Z
SUMMARY:Override instance
END:VEVENT
BEGIN:VEVENT
UID:series-1
DTSTART:20250611T120000Z
DTEND:20250611T130000Z
SUMMARY:Series master
END:VEVENT
END:VCALENDAR
`
	cal := decodeCalendar(t, recurring)
	e, ok := eventFromObject("cal", "/series.ics", cal)
	if !ok {
		t.Fatal("eventFromObject returned ok=false")
	}
	if e.Subject != "Series master" {
		t.Errorf("Subject = %q, want %q (must pick the master VEVENT, not the override)", e.Subject, "Series master")
	}
}

// TestEventObjectNameRejectsUnsafe guards the write-path: a caller-supplied UID
// must not be able to escape the calendar collection via the object filename.
func TestEventObjectNameRejectsUnsafe(t *testing.T) {
	for _, uid := range []string{"../../evil", "a/b", "..", ".", `x\y`, "has\x00null", "line\r\nbreak"} {
		if _, err := eventObjectName(uid); err == nil {
			t.Errorf("eventObjectName(%q) = nil, want rejection", uid)
		}
	}
	for _, uid := range []string{"abc123@go-mailbox-720", "simple-uid", "UID.with.dots"} {
		name, err := eventObjectName(uid)
		if err != nil {
			t.Errorf("eventObjectName(%q) = %v, want ok", uid, err)
		}
		if name != uid+".ics" {
			t.Errorf("eventObjectName(%q) = %q, want %q", uid, name, uid+".ics")
		}
	}
}

// TestEventToICalSanitizesCN guards against iCalendar injection: CR/LF in a
// display name (set as the CN parameter, which go-ical does not escape) must not
// inject forged property lines into the encoded object.
func TestEventToICalSanitizesCN(t *testing.T) {
	e := calendar.Event{
		UID:       "uid-1",
		Subject:   "Meeting",
		Start:     time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC),
		Organizer: calendar.Address{Name: "Evil\r\nX-INJECTED:yes\r\nORGANIZER;CN=foo", Email: "evil@example.com"},
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(eventToICal(e)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A forged property line would be a CRLF immediately followed by the property
	// name; go-ical line folding only ever emits CRLF + a space, so this pattern
	// can only come from un-sanitized CR/LF in the CN value.
	if out := buf.String(); strings.Contains(out, "\r\nX-INJECTED") {
		t.Errorf("CR/LF in organizer CN injected a forged property line:\n%s", out)
	}
}

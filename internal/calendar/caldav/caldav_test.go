package caldav

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/hstern/go-jscalendar"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// recordingHTTPClient is a webdav.HTTPClient that records the Authorization
// header of the last request and returns an empty 207 Multi-Status.
type recordingHTTPClient struct{ auth string }

func (r *recordingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	r.auth = req.Header.Get("Authorization")
	return &http.Response{StatusCode: http.StatusMultiStatus, Body: http.NoBody, Header: make(http.Header)}, nil
}

// Dial with Options.BearerToken authenticates with Authorization: Bearer, not
// Basic — the per-identity path (MB720-44).
func TestDialBearerAuth(t *testing.T) {
	rec := &recordingHTTPClient{}
	cl, err := Dial("http://dav.example/", "ignored-user", "ignored-pass",
		&Options{HTTPClient: rec, BearerToken: "tok-9"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Issue a request through the adapter's wrapped client (cl.http carries the
	// auth wrapper Dial installed).
	req, _ := http.NewRequest("PROPFIND", "http://dav.example/", nil)
	if _, err := cl.http.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if rec.auth != "Bearer tok-9" {
		t.Errorf("Authorization = %q, want %q (Bearer, not Basic)", rec.auth, "Bearer tok-9")
	}
}

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

	if e.Title != "Quarterly Planning" {
		t.Errorf("Title = %q, want %q", e.Title, "Quarterly Planning")
	}
	if e.UID != "event-123@example.com" {
		t.Errorf("UID = %q, want %q", e.UID, "event-123@example.com")
	}
	if loc := eventLocation(e); loc != "Conference Room B" {
		t.Errorf("Location = %q, want %q", loc, "Conference Room B")
	}
	if e.Status != "confirmed" {
		t.Errorf("Status = %q, want %q", e.Status, "confirmed")
	}
	if e.Description != "Discuss roadmap and OKRs." {
		t.Errorf("Description = %q, want %q", e.Description, "Discuss roadmap and OKRs.")
	}
	if e.ShowWithoutTime {
		t.Error("ShowWithoutTime = true, want false for a timed event")
	}

	wantStart := time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC)
	if !e.StartTime().Equal(wantStart) {
		t.Errorf("Start = %v, want %v", e.StartTime(), wantStart)
	}
	wantEnd := time.Date(2026, 6, 19, 14, 30, 0, 0, time.UTC)
	if !e.EndTime().Equal(wantEnd) {
		t.Errorf("End = %v, want %v", e.EndTime(), wantEnd)
	}
	wantCreated := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	if e.Created == nil || !e.Created.Time().Equal(wantCreated) {
		t.Errorf("Created = %v, want %v", e.Created, wantCreated)
	}

	org, ok := e.Organizer()
	if !ok {
		t.Fatal("Organizer() found = false, want the ORGANIZER participant")
	}
	if org.Name != "Alice Smith" || calendar.ParticipantEmail(org) != "alice@example.com" {
		t.Errorf("Organizer = name=%q email=%q, want Alice Smith/alice@example.com", org.Name, calendar.ParticipantEmail(org))
	}
	atts := e.Attendees()
	if len(atts) != 2 {
		t.Fatalf("got %d attendees, want 2", len(atts))
	}
	if atts[0].Name != "Bob Jones" || calendar.ParticipantEmail(atts[0]) != "bob@example.com" {
		t.Errorf("Attendees[0] = name=%q email=%q", atts[0].Name, calendar.ParticipantEmail(atts[0]))
	}
	if atts[1].Name != "" || calendar.ParticipantEmail(atts[1]) != "carol@example.com" {
		t.Errorf("Attendees[1] = name=%q email=%q", atts[1].Name, calendar.ParticipantEmail(atts[1]))
	}
}

// eventLocation returns the first location's name from the JSCalendar Locations
// map, the read-side counterpart of iCalendar's single LOCATION property.
func eventLocation(e calendar.Event) string {
	for _, k := range sortedLocationKeys(e) {
		return e.Locations[k].Name
	}
	return ""
}

func sortedLocationKeys(e calendar.Event) []jscalendar.Id {
	keys := make([]jscalendar.Id, 0, len(e.Locations))
	for k := range e.Locations {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
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

	if !e.ShowWithoutTime {
		t.Error("ShowWithoutTime = false, want true for a VALUE=DATE event")
	}
	if e.Title != "Company Holiday" {
		t.Errorf("Title = %q", e.Title)
	}
	wantStart := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	if !e.StartTime().Equal(wantStart) {
		t.Errorf("Start = %v, want %v", e.StartTime(), wantStart)
	}
	// Absent optional fields map to zero values, not errors.
	if eventLocation(e) != "" || e.Status != "" || e.Description != "" {
		t.Errorf("expected empty optional fields, got Location=%q Status=%q Description=%q", eventLocation(e), e.Status, e.Description)
	}
	if _, ok := e.Organizer(); ok {
		t.Error("Organizer() found = true, want none")
	}
	if len(e.Attendees()) != 0 {
		t.Errorf("Attendees = %+v, want none", e.Attendees())
	}
}

func TestEventFromObjectSetsIDs(t *testing.T) {
	cal := decodeCalendar(t, timedEvent)
	objectPath := "/calendars/alice/work/event-123.ics"
	calID := calendarID("/calendars/alice/work/")

	e, ok := eventFromObject(calID, objectPath, `"etag-123"`, cal)
	if !ok {
		t.Fatal("eventFromObject returned ok=false")
	}
	if e.ID != eventID(objectPath) {
		t.Errorf("ID = %q, want %q", e.ID, eventID(objectPath))
	}
	if e.CalendarID != calID {
		t.Errorf("CalendarID = %q, want %q", e.CalendarID, calID)
	}
	if e.ETag != `"etag-123"` {
		t.Errorf("ETag = %q, want %q", e.ETag, `"etag-123"`)
	}
}

func TestEventFromObjectNilCalendar(t *testing.T) {
	if _, ok := eventFromObject("cal", "/p.ics", "", nil); ok {
		t.Error("eventFromObject(nil) ok = true, want false")
	}
}

// eventToICal builds the VEVENT the write path PUTs; round-tripping it back
// through the read-path mapEvent must recover the event's fields, and the
// encoded form must carry the required structural properties.
func TestEventToICalRoundTrip(t *testing.T) {
	want := calendar.Event{Event: jscalendar.Event{
		UID:         "event-123@example.com",
		Title:       "Quarterly Planning",
		Sequence:    3,
		Description: "Discuss roadmap and OKRs.",
		Locations:   map[jscalendar.Id]jscalendar.Location{"1": {Type: "Location", Name: "Conference Room B"}},
	}}
	want.SetUTCTimes(
		time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 19, 14, 30, 0, 0, time.UTC),
	)
	org := calendar.NewParticipant("Alice Smith", "alice@example.com", "", "owner")
	// Attendee PARTSTAT round-trips (written as PARTSTAT=ACCEPTED, read back as the
	// JSCalendar "accepted" participationStatus).
	bob := calendar.NewParticipant("Bob Jones", "bob@example.com", "accepted", "attendee")
	carol := calendar.NewParticipant("", "carol@example.com", "", "attendee")
	want.SetOrganizerAttendees(&org, []jscalendar.Participant{bob, carol})

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

	if got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	if got.UID != want.UID {
		t.Errorf("UID = %q, want %q", got.UID, want.UID)
	}
	if eventLocation(got) != "Conference Room B" {
		t.Errorf("Location = %q, want %q", eventLocation(got), "Conference Room B")
	}
	if got.Sequence != want.Sequence {
		t.Errorf("Sequence = %d, want %d", got.Sequence, want.Sequence)
	}
	if !got.StartTime().Equal(want.StartTime()) {
		t.Errorf("Start = %v, want %v", got.StartTime(), want.StartTime())
	}
	if !got.EndTime().Equal(want.EndTime()) {
		t.Errorf("End = %v, want %v", got.EndTime(), want.EndTime())
	}
	if got.Description != want.Description {
		t.Errorf("Description = %q, want %q", got.Description, want.Description)
	}
	gotOrg, ok := got.Organizer()
	if !ok || gotOrg.Name != "Alice Smith" || calendar.ParticipantEmail(gotOrg) != "alice@example.com" {
		t.Errorf("Organizer = %+v (ok=%v)", gotOrg, ok)
	}
	gotAtts := got.Attendees()
	if len(gotAtts) != 2 {
		t.Fatalf("got %d attendees, want 2", len(gotAtts))
	}
	if gotAtts[0].Name != "Bob Jones" || calendar.ParticipantEmail(gotAtts[0]) != "bob@example.com" || gotAtts[0].ParticipationStatus != "accepted" {
		t.Errorf("Attendees[0] = name=%q email=%q partStat=%q", gotAtts[0].Name, calendar.ParticipantEmail(gotAtts[0]), gotAtts[0].ParticipationStatus)
	}
	if calendar.ParticipantEmail(gotAtts[1]) != "carol@example.com" {
		t.Errorf("Attendees[1] email = %q, want carol@example.com", calendar.ParticipantEmail(gotAtts[1]))
	}
}

// An attendee's RFC 6638 SCHEDULE-STATUS — the client-side scheduling delivery
// outcome the server records onto the participant — must round-trip through the
// CalDAV store so a later read recovers it. The go-jscalendar/ical bridge drops
// it, so the adapter emits/reads the SCHEDULE-STATUS ATTENDEE parameter directly.
func TestEventToICalScheduleStatusRoundTrip(t *testing.T) {
	want := calendar.Event{Event: jscalendar.Event{UID: "sched@example.com", Title: "Sync"}}
	want.SetUTCTimes(time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC), time.Date(2026, 6, 20, 9, 30, 0, 0, time.UTC))
	org := calendar.NewParticipant("Alice", "alice@example.com", "", "owner")
	bob := calendar.NewParticipant("Bob", "bob@example.com", "accepted", "attendee")
	bob.ScheduleStatus = []string{"1.1"} // RFC 6638 REQUEST-STATUS: message sent
	want.SetOrganizerAttendees(&org, []jscalendar.Participant{bob})

	cal := eventToICal(want)
	var buf strings.Builder
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(buf.String(), "SCHEDULE-STATUS=1.1") {
		t.Errorf("encoded ATTENDEE missing SCHEDULE-STATUS=1.1:\n%s", buf.String())
	}

	events := cal.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := mapEvent(&events[0])
	gotAtts := got.Attendees()
	if len(gotAtts) != 1 {
		t.Fatalf("got %d attendees, want 1", len(gotAtts))
	}
	if ss := gotAtts[0].ScheduleStatus; len(ss) != 1 || ss[0] != "1.1" {
		t.Errorf("ScheduleStatus = %v, want [1.1]", ss)
	}
}

// CreateEvent mints a UID when the input event has none; eventToICal must then
// still emit a UID (CalDAV resources require one).
func TestEventToICalMintedUID(t *testing.T) {
	cal := eventToICal(calendar.Event{Event: jscalendar.Event{UID: newUID(), Title: "No UID supplied"}})
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
	e, ok := eventFromObject("cal", "/series.ics", "", cal)
	if !ok {
		t.Fatal("eventFromObject returned ok=false")
	}
	if e.Title != "Series master" {
		t.Errorf("Title = %q, want %q (must pick the master VEVENT, not the override)", e.Title, "Series master")
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
	e := calendar.Event{Event: jscalendar.Event{UID: "uid-1", Title: "Meeting"}}
	e.SetUTCTimes(
		time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC),
	)
	org := calendar.NewParticipant("Evil\r\nX-INJECTED:yes\r\nORGANIZER;CN=foo", "evil@example.com", "", "owner")
	e.SetOrganizerAttendees(&org, nil)
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

package caldav

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

const weeklySeries = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:standup@example.com
SUMMARY:Daily Standup
DTSTART:20260601T090000Z
DTEND:20260601T091500Z
RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4
END:VEVENT
END:VCALENDAR
`

// weeklySeriesWithException drops the 2026-06-15 occurrence (EXDATE) and overrides
// the 2026-06-22 occurrence to a different time/summary (a RECURRENCE-ID VEVENT).
const weeklySeriesWithException = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//EN
BEGIN:VEVENT
UID:standup@example.com
SUMMARY:Daily Standup
DTSTART:20260601T090000Z
DTEND:20260601T091500Z
RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4
EXDATE:20260615T090000Z
END:VEVENT
BEGIN:VEVENT
UID:standup@example.com
RECURRENCE-ID:20260622T090000Z
SUMMARY:Standup (moved)
DTSTART:20260622T103000Z
DTEND:20260622T104500Z
END:VEVENT
END:VCALENDAR
`

func TestRecurrenceFromEvent(t *testing.T) {
	cal := decodeCalendar(t, weeklySeriesWithException)
	events := cal.Events()
	master := &events[0]

	pat := recurrenceFromEvent(master)
	if pat == nil {
		t.Fatal("recurrenceFromEvent returned nil for a series master")
	}
	if pat.RRULE != "FREQ=WEEKLY;BYDAY=MO;COUNT=4" {
		t.Errorf("RRULE = %q", pat.RRULE)
	}
	if len(pat.ExceptionDates) != 1 {
		t.Fatalf("got %d EXDATEs, want 1", len(pat.ExceptionDates))
	}
	wantEx := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	if !pat.ExceptionDates[0].Equal(wantEx) {
		t.Errorf("EXDATE = %v, want %v", pat.ExceptionDates[0], wantEx)
	}
}

func TestRecurrenceFromEventNonRecurring(t *testing.T) {
	cal := decodeCalendar(t, timedEvent)
	events := cal.Events()
	if pat := recurrenceFromEvent(&events[0]); pat != nil {
		t.Errorf("recurrenceFromEvent = %+v, want nil for a non-recurring event", pat)
	}
}

// mapEvent on a master carries the recurrence pattern; eventFromObject keeps the
// master as the collection-level representation while preserving the pattern.
func TestMapEventCarriesRecurrence(t *testing.T) {
	cal := decodeCalendar(t, weeklySeries)
	e, ok := eventFromObject("cal", "/c/standup.ics", cal)
	if !ok {
		t.Fatal("eventFromObject ok=false")
	}
	if e.Recurrence == nil {
		t.Fatal("master Event.Recurrence is nil")
	}
	if e.RecurrenceID.IsZero() == false {
		t.Error("master Event.RecurrenceID should be zero")
	}
}

func TestExpandInstances(t *testing.T) {
	cal := decodeCalendar(t, weeklySeries)
	r := calendar.Range{
		Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	insts, err := expandInstances("cal", "/c/standup.ics", cal, r)
	if err != nil {
		t.Fatalf("expandInstances: %v", err)
	}
	// COUNT=4 weekly from Mon 2026-06-01: 06-01, 06-08, 06-15, 06-22.
	if len(insts) != 4 {
		t.Fatalf("got %d instances, want 4", len(insts))
	}
	want := []time.Time{
		time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC),
	}
	for i, w := range want {
		if !insts[i].Start.Equal(w) {
			t.Errorf("instance %d Start = %v, want %v", i, insts[i].Start, w)
		}
		if !insts[i].RecurrenceID.Equal(w) {
			t.Errorf("instance %d RecurrenceID = %v, want %v", i, insts[i].RecurrenceID, w)
		}
		if insts[i].SeriesMasterID != eventID("/c/standup.ics") {
			t.Errorf("instance %d SeriesMasterID = %q", i, insts[i].SeriesMasterID)
		}
		// Each occurrence keeps the master's 15-minute duration.
		if got := insts[i].End.Sub(insts[i].Start); got != 15*time.Minute {
			t.Errorf("instance %d duration = %v, want 15m", i, got)
		}
		if insts[i].Recurrence != nil {
			t.Errorf("instance %d carries a recurrence pattern; an occurrence is not a series", i)
		}
	}
}

func TestExpandInstancesWithExceptionAndOverride(t *testing.T) {
	cal := decodeCalendar(t, weeklySeriesWithException)
	r := calendar.Range{
		Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	insts, err := expandInstances("cal", "/c/standup.ics", cal, r)
	if err != nil {
		t.Fatalf("expandInstances: %v", err)
	}
	// EXDATE removes 06-15; the remaining occurrences are 06-01, 06-08, 06-22.
	if len(insts) != 3 {
		t.Fatalf("got %d instances, want 3: %+v", len(insts), insts)
	}
	for _, e := range insts {
		if e.RecurrenceID.Equal(time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)) {
			t.Error("EXDATE-cancelled occurrence 06-15 should be omitted")
		}
	}
	// The 06-22 occurrence takes the override's moved time and summary.
	last := insts[2]
	if !last.RecurrenceID.Equal(time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last RecurrenceID = %v", last.RecurrenceID)
	}
	if !last.Start.Equal(time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("override Start = %v, want 10:30", last.Start)
	}
	if last.Subject != "Standup (moved)" {
		t.Errorf("override Subject = %q", last.Subject)
	}
	if !last.IsOverride {
		t.Error("override instance IsOverride = false")
	}
}

func TestInstanceEventIDRoundTrip(t *testing.T) {
	objectPath := "/calendars/alice/work/standup.ics"
	rid := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)

	id := instanceEventID(objectPath, rid)
	if id == eventID(objectPath) {
		t.Error("instance id collides with the master id")
	}

	gotPath, gotRID, ok, err := decodeInstanceEventID(id)
	if err != nil {
		t.Fatalf("decodeInstanceEventID: %v", err)
	}
	if !ok {
		t.Fatal("decodeInstanceEventID ok=false for an instance id")
	}
	if gotPath != objectPath {
		t.Errorf("path = %q, want %q", gotPath, objectPath)
	}
	if !gotRID.Equal(rid) {
		t.Errorf("recurrence-id = %v, want %v", gotRID, rid)
	}
}

func TestDecodeInstanceEventIDPlainID(t *testing.T) {
	objectPath := "/calendars/alice/work/event-123.ics"
	id := eventID(objectPath)

	gotPath, _, ok, err := decodeInstanceEventID(id)
	if err != nil {
		t.Fatalf("decodeInstanceEventID: %v", err)
	}
	if ok {
		t.Error("plain master id decoded as an instance id")
	}
	if gotPath != objectPath {
		t.Errorf("path = %q, want %q", gotPath, objectPath)
	}
}

func TestInstanceFromObjectOverride(t *testing.T) {
	cal := decodeCalendar(t, weeklySeriesWithException)
	rid := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)

	e, ok := instanceFromObject("cal", "/c/standup.ics", cal, rid)
	if !ok {
		t.Fatal("instanceFromObject ok=false")
	}
	if !e.IsOverride {
		t.Error("override instance IsOverride = false")
	}
	if e.Subject != "Standup (moved)" {
		t.Errorf("Subject = %q", e.Subject)
	}
	if !e.Start.Equal(time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("Start = %v", e.Start)
	}
}

func TestInstanceFromObjectSynthesized(t *testing.T) {
	cal := decodeCalendar(t, weeklySeries)
	rid := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)

	e, ok := instanceFromObject("cal", "/c/standup.ics", cal, rid)
	if !ok {
		t.Fatal("instanceFromObject ok=false")
	}
	if e.IsOverride {
		t.Error("synthesized occurrence IsOverride = true")
	}
	if !e.Start.Equal(rid) {
		t.Errorf("Start = %v, want %v", e.Start, rid)
	}
	if got := e.End.Sub(e.Start); got != 15*time.Minute {
		t.Errorf("duration = %v, want 15m", got)
	}
	if e.SeriesMasterID != eventID("/c/standup.ics") {
		t.Errorf("SeriesMasterID = %q", e.SeriesMasterID)
	}
}

// The write path emits RRULE + EXDATE for a series master and round-trips it.
func TestEventToICalRecurrence(t *testing.T) {
	master := calendar.Event{
		UID:     "standup@example.com",
		Subject: "Daily Standup",
		Start:   time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 6, 1, 9, 15, 0, 0, time.UTC),
		Recurrence: &calendar.RecurrencePattern{
			RRULE:          "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
			ExceptionDates: []time.Time{time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)},
		},
	}

	cal := eventToICal(master)
	var buf strings.Builder
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4") {
		t.Errorf("encoded object missing RRULE:\n%s", out)
	}
	if !strings.Contains(out, "EXDATE") {
		t.Errorf("encoded object missing EXDATE:\n%s", out)
	}

	// Round-trip back through mapEvent.
	events := cal.Events()
	got := mapEvent(&events[0])
	if got.Recurrence == nil || got.Recurrence.RRULE != master.Recurrence.RRULE {
		t.Errorf("round-trip Recurrence = %+v", got.Recurrence)
	}
	if len(got.Recurrence.ExceptionDates) != 1 {
		t.Errorf("round-trip EXDATE count = %d", len(got.Recurrence.ExceptionDates))
	}
}

// An override event written by the write path carries a RECURRENCE-ID that maps
// back to RecurrenceID + IsOverride.
func TestEventToICalOverride(t *testing.T) {
	override := calendar.Event{
		UID:          "standup@example.com",
		Subject:      "Standup (moved)",
		Start:        time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC),
		End:          time.Date(2026, 6, 22, 10, 45, 0, 0, time.UTC),
		RecurrenceID: time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC),
	}
	cal := eventToICal(override)
	var buf strings.Builder
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(buf.String(), "RECURRENCE-ID") {
		t.Errorf("encoded override missing RECURRENCE-ID:\n%s", buf.String())
	}

	events := cal.Events()
	got := mapEvent(&events[0])
	if !got.IsOverride {
		t.Error("round-trip IsOverride = false")
	}
	if !got.RecurrenceID.Equal(override.RecurrenceID) {
		t.Errorf("round-trip RecurrenceID = %v, want %v", got.RecurrenceID, override.RecurrenceID)
	}
}

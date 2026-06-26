package caldav

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/hstern/go-jscalendar"

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

// mapEvent routes the master VEVENT's RRULE through the ical bridge into the
// structured JSCalendar RecurrenceRules. (EXDATE is no longer surfaced on the
// envelope — the bridge does not map per-date exceptions — but recurrence
// expansion still folds it in via go-ical's RecurrenceSet; see TestExpandInstances
// WithExceptionAndOverride.)
func TestRecurrenceFromEvent(t *testing.T) {
	cal := decodeCalendar(t, weeklySeriesWithException)
	events := cal.Events()
	e := mapEvent(&events[0])

	if len(e.RecurrenceRules) != 1 {
		t.Fatalf("got %d recurrence rules, want 1", len(e.RecurrenceRules))
	}
	rule := e.RecurrenceRules[0]
	if rule.Frequency != jscalendar.FrequencyWeekly {
		t.Errorf("Frequency = %q, want weekly", rule.Frequency)
	}
	if rule.Count == nil || *rule.Count != 4 {
		t.Errorf("Count = %v, want 4", rule.Count)
	}
	if len(rule.ByDay) != 1 || rule.ByDay[0].Day != "mo" {
		t.Errorf("ByDay = %+v, want [mo]", rule.ByDay)
	}
}

func TestRecurrenceFromEventNonRecurring(t *testing.T) {
	cal := decodeCalendar(t, timedEvent)
	events := cal.Events()
	if e := mapEvent(&events[0]); len(e.RecurrenceRules) != 0 {
		t.Errorf("RecurrenceRules = %+v, want none for a non-recurring event", e.RecurrenceRules)
	}
}

// mapEvent on a master carries the recurrence rules; eventFromObject keeps the
// master as the collection-level representation while preserving the rules.
func TestMapEventCarriesRecurrence(t *testing.T) {
	cal := decodeCalendar(t, weeklySeries)
	e, ok := eventFromObject("cal", "/c/standup.ics", "", cal)
	if !ok {
		t.Fatal("eventFromObject ok=false")
	}
	if len(e.RecurrenceRules) == 0 {
		t.Fatal("master Event.RecurrenceRules is empty")
	}
	if e.RecurrenceID != nil {
		t.Error("master Event.RecurrenceID should be nil")
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
		if !insts[i].StartTime().Equal(w) {
			t.Errorf("instance %d Start = %v, want %v", i, insts[i].StartTime(), w)
		}
		if !recurrenceIDTime(insts[i]).Equal(w) {
			t.Errorf("instance %d RecurrenceID = %v, want %v", i, recurrenceIDTime(insts[i]), w)
		}
		if insts[i].SeriesMasterID != eventID("/c/standup.ics") {
			t.Errorf("instance %d SeriesMasterID = %q", i, insts[i].SeriesMasterID)
		}
		// Each occurrence keeps the master's 15-minute duration.
		if got := insts[i].EndTime().Sub(insts[i].StartTime()); got != 15*time.Minute {
			t.Errorf("instance %d duration = %v, want 15m", i, got)
		}
		if len(insts[i].RecurrenceRules) != 0 {
			t.Errorf("instance %d carries recurrence rules; an occurrence is not a series", i)
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
		if recurrenceIDTime(e).Equal(time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)) {
			t.Error("EXDATE-cancelled occurrence 06-15 should be omitted")
		}
	}
	// The 06-22 occurrence takes the override's moved time and summary.
	last := insts[2]
	if !recurrenceIDTime(last).Equal(time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last RecurrenceID = %v", recurrenceIDTime(last))
	}
	if !last.StartTime().Equal(time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("override Start = %v, want 10:30", last.StartTime())
	}
	if last.Title != "Standup (moved)" {
		t.Errorf("override Title = %q", last.Title)
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

	e, ok := instanceFromObject("cal", "/c/standup.ics", "", cal, rid)
	if !ok {
		t.Fatal("instanceFromObject ok=false")
	}
	if !e.IsOverride {
		t.Error("override instance IsOverride = false")
	}
	if e.Title != "Standup (moved)" {
		t.Errorf("Title = %q", e.Title)
	}
	if !e.StartTime().Equal(time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("Start = %v", e.StartTime())
	}
}

func TestInstanceFromObjectSynthesized(t *testing.T) {
	cal := decodeCalendar(t, weeklySeries)
	rid := time.Date(2026, 6, 8, 9, 0, 0, 0, time.UTC)

	e, ok := instanceFromObject("cal", "/c/standup.ics", "", cal, rid)
	if !ok {
		t.Fatal("instanceFromObject ok=false")
	}
	if e.IsOverride {
		t.Error("synthesized occurrence IsOverride = true")
	}
	if !e.StartTime().Equal(rid) {
		t.Errorf("Start = %v, want %v", e.StartTime(), rid)
	}
	if got := e.EndTime().Sub(e.StartTime()); got != 15*time.Minute {
		t.Errorf("duration = %v, want 15m", got)
	}
	if e.SeriesMasterID != eventID("/c/standup.ics") {
		t.Errorf("SeriesMasterID = %q", e.SeriesMasterID)
	}
}

// The write path emits RRULE (from the structured rules) + EXDATE (from an
// "excluded":true recurrence override) for a series master and round-trips the
// RRULE back. EXDATE is no longer re-read into the envelope (the bridge does not
// map per-date exceptions), so the round-trip check is on the encoded EXDATE and
// the recovered RRULE only.
func TestEventToICalRecurrence(t *testing.T) {
	rules, err := calendar.RulesFromRRULE("FREQ=WEEKLY;BYDAY=MO;COUNT=4")
	if err != nil {
		t.Fatalf("RulesFromRRULE: %v", err)
	}
	master := calendar.Event{Event: jscalendar.Event{
		UID:             "standup@example.com",
		Title:           "Daily Standup",
		RecurrenceRules: rules,
		RecurrenceOverrides: map[string]jscalendar.PatchObject{
			// 2026-06-15T09:00:00, the EXDATE-cancelled occurrence.
			"2026-06-15T09:00:00": {"excluded": json.RawMessage("true")},
		},
	}}
	master.SetUTCTimes(
		time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 9, 15, 0, 0, time.UTC),
	)

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

	// Round-trip the RRULE back through mapEvent.
	events := cal.Events()
	got := mapEvent(&events[0])
	if len(got.RecurrenceRules) != 1 {
		t.Fatalf("round-trip RecurrenceRules = %+v, want 1", got.RecurrenceRules)
	}
	if got.RecurrenceRules[0].Frequency != jscalendar.FrequencyWeekly {
		t.Errorf("round-trip Frequency = %q", got.RecurrenceRules[0].Frequency)
	}
}

// An override event written by the write path carries a RECURRENCE-ID that maps
// back to RecurrenceID + IsOverride.
func TestEventToICalOverride(t *testing.T) {
	override := calendar.Event{Event: jscalendar.Event{
		UID:   "standup@example.com",
		Title: "Standup (moved)",
	}}
	override.SetUTCTimes(
		time.Date(2026, 6, 22, 10, 30, 0, 0, time.UTC),
		time.Date(2026, 6, 22, 10, 45, 0, 0, time.UTC),
	)
	wantRID := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	setRecurrenceID(&override, wantRID)

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
	if !recurrenceIDTime(got).Equal(wantRID) {
		t.Errorf("round-trip RecurrenceID = %v, want %v", recurrenceIDTime(got), wantRID)
	}
}

package jmap

import (
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-jscalendar"
	jscal "github.com/hstern/go-jscalendar/jmap"

	calendar "github.com/hstern/go-mailbox-720/internal/calendar"
)

func TestToCalendarEventScalars(t *testing.T) {
	ev := &jscalendar.Event{
		UID:         "uid-1",
		Title:       "Standup",
		Status:      "confirmed",
		Description: "daily",
		Sequence:    2,
	}
	ce := jscal.FromEvent(ev)
	ce.ID = "e1"
	ce.CalendarIDs = map[jscalendar.Id]bool{"c1": true}

	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}
	if got.ID != "e1" || got.CalendarID != "c1" {
		t.Fatalf("ids: %+v", got)
	}
	if got.UID != "uid-1" || got.Subject != "Standup" || got.Status != "confirmed" {
		t.Fatalf("fields: %+v", got)
	}
	if got.Body.Content != "daily" || got.Sequence != 2 {
		t.Fatalf("body/seq: %+v", got)
	}
}

func TestToCalendarEventRecurrenceRRULE(t *testing.T) {
	ev := &jscalendar.Event{
		UID:   "uid-2",
		Title: "Weekly",
		Start: &jscalendar.LocalDateTime{Year: 2026, Month: 1, Day: 5, Hour: 9},
		RecurrenceRules: []jscalendar.RecurrenceRule{
			{Frequency: "weekly", ByDay: []jscalendar.NDay{{Day: "mo"}}},
		},
	}
	ce := jscal.FromEvent(ev)
	ce.ID = "e2"
	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}
	if got.Recurrence == nil || got.Recurrence.RRULE == "" {
		t.Fatalf("recurrence not mapped: %+v", got.Recurrence)
	}
	// RRULE string should carry the weekly Monday rule.
	if want := "FREQ=WEEKLY"; !strings.Contains(got.Recurrence.RRULE, want) {
		t.Fatalf("RRULE %q missing %q", got.Recurrence.RRULE, want)
	}
}

// TestToCalendarEventStartEndLocationAttendees exercises the previously-untested
// Start, End (via Duration), IsAllDay, Location, and Attendees fields.
// It also asserts that two-participant events produce a deterministic Attendees
// slice regardless of map iteration order.
func TestToCalendarEventStartEndLocationAttendees(t *testing.T) {
	dur := jscalendar.Duration{Hours: 1, Minutes: 30}
	ev := &jscalendar.Event{
		UID:             "uid-3",
		ShowWithoutTime: true,
		Start:           &jscalendar.LocalDateTime{Year: 2026, Month: 6, Day: 15, Hour: 10},
		Duration:        &dur,
		Locations: map[jscalendar.Id]jscalendar.Location{
			"loc1": {Name: "Room A"},
		},
		Participants: map[jscalendar.Id]jscalendar.Participant{
			// Two participants with keys that sort "p2" before "p1" — ensures
			// we are actually sorting, not just getting lucky with map order.
			"p2": {Name: "Bob", Email: "bob@example.com", Roles: map[string]bool{"attendee": true}, ParticipationStatus: "accepted"},
			"p1": {Name: "Alice", Email: "alice@example.com", Roles: map[string]bool{"owner": true, "attendee": true}, ParticipationStatus: "accepted"},
		},
	}
	ce := jscal.FromEvent(ev)
	ce.ID = "e3"

	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}

	// Start should be 2026-06-15T10:00:00Z (floating, treated as UTC).
	wantStart := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	if !got.Start.Equal(wantStart) {
		t.Errorf("Start: got %v, want %v", got.Start, wantStart)
	}

	// End = Start + 1h30m.
	wantEnd := wantStart.Add(90 * time.Minute)
	if !got.End.Equal(wantEnd) {
		t.Errorf("End: got %v, want %v", got.End, wantEnd)
	}

	// IsAllDay maps ShowWithoutTime.
	if !got.IsAllDay {
		t.Errorf("IsAllDay: got false, want true")
	}

	// Location: the single location's Name.
	if got.Location != "Room A" {
		t.Errorf("Location: got %q, want %q", got.Location, "Room A")
	}

	// Attendees must be in sorted participant-key order: p1 (Alice) then p2 (Bob).
	if len(got.Attendees) != 2 {
		t.Fatalf("Attendees len: got %d, want 2", len(got.Attendees))
	}
	if got.Attendees[0].Email != "alice@example.com" {
		t.Errorf("Attendees[0].Email: got %q, want %q", got.Attendees[0].Email, "alice@example.com")
	}
	if got.Attendees[1].Email != "bob@example.com" {
		t.Errorf("Attendees[1].Email: got %q, want %q", got.Attendees[1].Email, "bob@example.com")
	}
	if got.Attendees[0].Status != "accepted" || got.Attendees[1].Status != "accepted" {
		t.Errorf("Attendees statuses: got %v/%v, want accepted/accepted",
			got.Attendees[0].Status, got.Attendees[1].Status)
	}

	// Organizer should be the owner participant (Alice, key p1).
	if got.Organizer.Email != "alice@example.com" {
		t.Errorf("Organizer.Email: got %q, want %q", got.Organizer.Email, "alice@example.com")
	}
}

func TestFromCalendarEventRoundTrip(t *testing.T) {
	in := calendar.Event{
		UID:        "uid-3",
		CalendarID: "c1",
		Subject:    "Review",
		Status:     "tentative",
		Body:       calendar.Body{ContentType: "text", Content: "notes"},
	}
	ce, err := fromCalendarEvent(in)
	if err != nil {
		t.Fatalf("fromCalendarEvent: %v", err)
	}
	if ce.UID != "uid-3" || ce.Title != "Review" || ce.Status != "tentative" {
		t.Fatalf("fields: %+v", ce.Event)
	}
	if !ce.CalendarIDs["c1"] {
		t.Fatalf("calendarIds: %+v", ce.CalendarIDs)
	}
}

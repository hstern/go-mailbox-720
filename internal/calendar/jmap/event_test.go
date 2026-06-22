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
	if got.UID != "uid-1" || got.Title != "Standup" || got.Status != "confirmed" {
		t.Fatalf("fields: %+v", got)
	}
	if got.Description != "daily" || got.Sequence != 2 {
		t.Fatalf("description/seq: %+v", got)
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
	// The structured recurrence rules carry over verbatim on the embedded Event.
	if len(got.RecurrenceRules) != 1 {
		t.Fatalf("recurrence not mapped: %+v", got.RecurrenceRules)
	}
	if got.RecurrenceRules[0].Frequency != "weekly" {
		t.Fatalf("frequency = %q, want weekly", got.RecurrenceRules[0].Frequency)
	}
	// And they still render to an RFC 5545 RRULE via the shared helper.
	rrule, err := calendar.RRULEFromRules(got.Start, got.TimeZone, got.RecurrenceRules)
	if err != nil {
		t.Fatalf("RRULEFromRules: %v", err)
	}
	if want := "FREQ=WEEKLY"; !strings.Contains(rrule, want) {
		t.Fatalf("RRULE %q missing %q", rrule, want)
	}
}

// TestToCalendarEventStartEndLocationAttendees exercises Start, End (via
// Duration), IsAllDay (ShowWithoutTime), Locations, and Participants on the
// embedded JSCalendar Event. It also asserts that two-participant events produce
// a deterministic Attendees slice regardless of map iteration order.
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
			// the shared accessors actually sort, not just get lucky with map order.
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

	// StartTime resolves to 2026-06-15T10:00:00Z (floating, treated as UTC).
	wantStart := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	if !got.StartTime().Equal(wantStart) {
		t.Errorf("StartTime: got %v, want %v", got.StartTime(), wantStart)
	}

	// EndTime = Start + 1h30m.
	wantEnd := wantStart.Add(90 * time.Minute)
	if !got.EndTime().Equal(wantEnd) {
		t.Errorf("EndTime: got %v, want %v", got.EndTime(), wantEnd)
	}

	// ShowWithoutTime carries the all-day flag.
	if !got.ShowWithoutTime {
		t.Errorf("ShowWithoutTime: got false, want true")
	}

	// Location: the single location's Name.
	if got.Locations["loc1"].Name != "Room A" {
		t.Errorf("Location: got %q, want %q", got.Locations["loc1"].Name, "Room A")
	}

	// Attendees must be in sorted participant-key order: p1 (Alice) then p2 (Bob).
	attendees := got.Attendees()
	if len(attendees) != 2 {
		t.Fatalf("Attendees len: got %d, want 2", len(attendees))
	}
	if calendar.ParticipantEmail(attendees[0]) != "alice@example.com" {
		t.Errorf("Attendees[0] email: got %q, want %q", calendar.ParticipantEmail(attendees[0]), "alice@example.com")
	}
	if calendar.ParticipantEmail(attendees[1]) != "bob@example.com" {
		t.Errorf("Attendees[1] email: got %q, want %q", calendar.ParticipantEmail(attendees[1]), "bob@example.com")
	}
	if attendees[0].ParticipationStatus != "accepted" || attendees[1].ParticipationStatus != "accepted" {
		t.Errorf("Attendees statuses: got %v/%v, want accepted/accepted",
			attendees[0].ParticipationStatus, attendees[1].ParticipationStatus)
	}

	// Organizer should be the owner participant (Alice, key p1).
	org, ok := got.Organizer()
	if !ok || calendar.ParticipantEmail(org) != "alice@example.com" {
		t.Errorf("Organizer email: got %q (ok=%v), want %q", calendar.ParticipantEmail(org), ok, "alice@example.com")
	}
}

// TestParticipantRoundTrip verifies that an Event with an Organizer and two
// Attendees survives fromCalendarEvent → toCalendarEvent with no data loss: the
// organizer holds the "owner" role and the two attendees hold "attendee".
func TestParticipantRoundTrip(t *testing.T) {
	organizer := calendar.NewParticipant("Org Person", "org@example.com", "", "owner")
	attendees := []jscalendar.Participant{
		calendar.NewParticipant("Alice Attendee", "alice@example.com", "accepted", "attendee"),
		calendar.NewParticipant("Bob Attendee", "bob@example.com", "declined", "attendee"),
	}
	in := calendar.Event{CalendarID: "c1"}
	in.UID = "uid-rt"
	in.Title = "Participant Round-Trip"
	in.SetOrganizerAttendees(&organizer, attendees)

	ce, err := fromCalendarEvent(in)
	if err != nil {
		t.Fatalf("fromCalendarEvent: %v", err)
	}
	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}

	// Organizer must be preserved.
	org, ok := got.Organizer()
	if !ok {
		t.Fatalf("organizer missing")
	}
	if calendar.ParticipantEmail(org) != "org@example.com" {
		t.Errorf("Organizer email: got %q, want %q", calendar.ParticipantEmail(org), "org@example.com")
	}
	if org.Name != "Org Person" {
		t.Errorf("Organizer name: got %q, want %q", org.Name, "Org Person")
	}

	// Exactly 2 attendees.
	gotAttendees := got.Attendees()
	if len(gotAttendees) != 2 {
		t.Fatalf("Attendees len: got %d, want 2", len(gotAttendees))
	}

	byEmail := make(map[string]jscalendar.Participant, len(gotAttendees))
	for _, a := range gotAttendees {
		byEmail[calendar.ParticipantEmail(a)] = a
	}
	if a, ok := byEmail["alice@example.com"]; !ok {
		t.Errorf("alice@example.com missing from Attendees")
	} else if a.ParticipationStatus != "accepted" {
		t.Errorf("alice status: got %q, want %q", a.ParticipationStatus, "accepted")
	}
	if a, ok := byEmail["bob@example.com"]; !ok {
		t.Errorf("bob@example.com missing from Attendees")
	} else if a.ParticipationStatus != "declined" {
		t.Errorf("bob status: got %q, want %q", a.ParticipationStatus, "declined")
	}
}

func TestFromCalendarEventRoundTrip(t *testing.T) {
	in := calendar.Event{CalendarID: "c1"}
	in.UID = "uid-3"
	in.Title = "Review"
	in.Status = "tentative"
	in.Description = "notes"

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
	if ce.Description != "notes" {
		t.Fatalf("description: %q", ce.Description)
	}
}

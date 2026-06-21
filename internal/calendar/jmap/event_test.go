package jmap

import (
	"strings"
	"testing"

	"github.com/hstern/go-jscalendar"
	jscal "github.com/hstern/go-jscalendar/jmap"
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

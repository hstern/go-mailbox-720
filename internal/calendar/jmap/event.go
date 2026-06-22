package jmap

import (
	"fmt"
	"sort"

	"github.com/hstern/go-jscalendar"
	jscal "github.com/hstern/go-jscalendar/jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// toCalendarEvent maps a JMAP CalendarEvent to the backend-neutral
// calendar.Event. Both types embed *jscalendar.Event / jscalendar.Event, so the
// content fields (Title, Start, Status, Participants, RecurrenceRules, …) carry
// over by value with no field-by-field glue: we copy the embedded Event and set
// only the backend-derived routing envelope (ID, CalendarID, SeriesMasterID,
// IsOverride) that is ours rather than JSCalendar's.
func toCalendarEvent(ce *jscal.CalendarEvent) (calendar.Event, error) {
	if ce == nil {
		return calendar.Event{}, fmt.Errorf("jmap: nil CalendarEvent")
	}

	ev := calendar.Event{ID: string(ce.ID)}

	// CalendarID: first key of CalendarIDs (sorted for determinism).
	if len(ce.CalendarIDs) > 0 {
		keys := make([]string, 0, len(ce.CalendarIDs))
		for k := range ce.CalendarIDs {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		ev.CalendarID = keys[0]
	}

	// SeriesMasterID from BaseEventID (set only on synthetic instance ids).
	if ce.BaseEventID != nil {
		ev.SeriesMasterID = string(*ce.BaseEventID)
	}

	// IsOverride: an instance carrying a RecurrenceID is an explicit exception.
	ev.IsOverride = ce.Event != nil && ce.RecurrenceID != nil

	if ce.Event == nil {
		return ev, nil
	}

	// Carry the JSCalendar content over by value.
	ev.Event = *ce.Event

	return ev, nil
}

// fromCalendarEvent maps a backend-neutral calendar.Event to a JMAP
// CalendarEvent, inverting toCalendarEvent. It wraps the embedded JSCalendar
// Event with jscal.FromEvent and stamps the routing envelope back onto the
// CalendarEvent's JMAP members.
func fromCalendarEvent(e calendar.Event) (*jscal.CalendarEvent, error) {
	ce := jscal.FromEvent(&e.Event)

	ce.ID = jscalendar.Id(e.ID)
	if e.CalendarID != "" {
		ce.CalendarIDs = map[jscalendar.Id]bool{jscalendar.Id(e.CalendarID): true}
	}
	if e.SeriesMasterID != "" {
		baseID := jscalendar.Id(e.SeriesMasterID)
		ce.BaseEventID = &baseID
	}

	return ce, nil
}

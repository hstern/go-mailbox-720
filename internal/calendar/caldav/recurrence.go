package caldav

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

var _ calendar.InstanceReader = (*Client)(nil)

// recurrenceFromEvent reads the recurrence pattern off a master VEVENT: its RRULE
// value plus any EXDATE exception instants. It returns nil when the VEVENT carries
// no RRULE (a non-recurring event), so a plain event maps to a nil
// Event.Recurrence.
func recurrenceFromEvent(ev *ical.Event) *calendar.RecurrencePattern {
	rrule := ev.Props.Get(ical.PropRecurrenceRule)
	if rrule == nil || strings.TrimSpace(rrule.Value) == "" {
		return nil
	}
	pat := &calendar.RecurrencePattern{RRULE: strings.TrimSpace(rrule.Value)}
	for _, ex := range ev.Props.Values(ical.PropExceptionDates) {
		if t, err := ex.DateTime(time.UTC); err == nil {
			pat.ExceptionDates = append(pat.ExceptionDates, t.UTC())
		}
	}
	return pat
}

// recurrenceIDOf returns the RECURRENCE-ID instant of an override VEVENT (the
// original start of the occurrence it replaces) and whether the VEVENT carries
// one. The zero time with ok=false means a master (non-override) component.
func recurrenceIDOf(ev *ical.Event) (time.Time, bool) {
	prop := ev.Props.Get(ical.PropRecurrenceID)
	if prop == nil {
		return time.Time{}, false
	}
	t, err := prop.DateTime(time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// ListInstances expands the recurring series whose master is identified by the
// opaque event ID into its occurrences within r, merging in any stored override
// (RECURRENCE-ID) VEVENTs. It implements calendar.InstanceReader, backing Graph's
// GET /me/events/{id}/instances and the occurrence expansion of /me/calendarView.
//
// The object resource holds the master plus every override under one UID; this
// fetches it once, expands the master's RRULE over [r.Start, r.End] (the window
// the caller must bound — an open-ended RRULE is infinite), drops EXDATE-cancelled
// and overridden-away occurrences, and substitutes each override's own fields. A
// non-recurring event yields itself when it falls in the window.
func (cl *Client) ListInstances(ctx context.Context, id string, r calendar.Range) ([]calendar.Event, error) {
	if r.Start.IsZero() || r.End.IsZero() {
		return nil, fmt.Errorf("caldav: ListInstances requires a bounded range")
	}
	objectPath, err := decodeEventID(id)
	if err != nil {
		return nil, err
	}
	obj, err := cl.c.GetCalendarObject(ctx, objectPath)
	if err != nil {
		return nil, fmt.Errorf("caldav: get calendar object %q: %w", objectPath, err)
	}
	calID := calendarIDForObject(objectPath)
	return expandInstances(calID, objectPath, obj.Data, r)
}

// expandInstances is the pure core of ListInstances: given a parsed calendar
// object (master + overrides), it produces the occurrence events in the window.
// Split out so it is testable without a CalDAV server.
func expandInstances(calID, objectPath string, cal *ical.Calendar, r calendar.Range) ([]calendar.Event, error) {
	if cal == nil {
		return nil, fmt.Errorf("caldav: nil calendar object")
	}
	events := cal.Events()
	if len(events) == 0 {
		return nil, fmt.Errorf("caldav: object %q has no VEVENT", objectPath)
	}

	masterIdx := -1
	overrides := make(map[time.Time]*ical.Event)
	for i := range events {
		if rid, ok := recurrenceIDOf(&events[i]); ok {
			overrides[rid] = &events[i]
			continue
		}
		if masterIdx < 0 {
			masterIdx = i
		}
	}
	if masterIdx < 0 {
		return nil, fmt.Errorf("caldav: object %q has no master VEVENT", objectPath)
	}
	master := &events[masterIdx]
	masterID := eventID(objectPath)

	set, err := master.RecurrenceSet(time.UTC)
	if err != nil {
		return nil, fmt.Errorf("caldav: expand recurrence: %w", err)
	}

	out := make([]calendar.Event, 0)
	if set == nil {
		// No RRULE: a single (possibly all-day) event. Surface it when it falls in
		// the window, addressed by its own master ID (it is not part of a series).
		base := mapEvent(master)
		if !base.Start.IsZero() && inWindow(base.Start, r) {
			base.ID = masterID
			base.CalendarID = calID
			out = append(out, base)
		}
		return out, nil
	}

	// Duration of a synthesized occurrence: the master's DTEND-DTSTART span.
	masterStart, _ := master.DateTimeStart(time.UTC)
	masterEnd, _ := master.DateTimeEnd(time.UTC)
	var dur time.Duration
	if !masterStart.IsZero() && !masterEnd.IsZero() {
		dur = masterEnd.Sub(masterStart)
	}

	// rrule.Set folds EXDATE in already (go-ical's RecurrenceSet wired it), so the
	// occurrences here are the rule minus its exceptions.
	occurrences := set.Between(r.Start, r.End, true)
	for _, occ := range occurrences {
		occ = occ.UTC()
		var inst calendar.Event
		if ov, ok := overrides[occ]; ok {
			// An override replaces this occurrence: take its fields. It may move the
			// occurrence outside the window; honor the override's own start.
			inst = mapEvent(ov)
			inst.IsOverride = true
		} else {
			inst = mapEvent(master)
			inst.Recurrence = nil // an occurrence is not itself a series
			inst.Start = occ
			if dur > 0 {
				inst.End = occ.Add(dur)
			}
		}
		inst.RecurrenceID = occ
		inst.ID = instanceEventID(objectPath, occ)
		inst.CalendarID = calID
		inst.SeriesMasterID = masterID
		out = append(out, inst)
	}

	// Overrides whose RECURRENCE-ID lies in the window but whose generating
	// occurrence was not produced (e.g. an EXDATE that the organizer replaced with
	// a moved instance) still need surfacing. Add any not already emitted.
	seen := make(map[time.Time]bool, len(out))
	for _, e := range out {
		seen[e.RecurrenceID] = true
	}
	for rid, ov := range overrides {
		if seen[rid] || !inWindow(rid, r) {
			continue
		}
		inst := mapEvent(ov)
		inst.IsOverride = true
		inst.RecurrenceID = rid
		inst.ID = instanceEventID(objectPath, rid)
		inst.CalendarID = calID
		inst.SeriesMasterID = masterID
		out = append(out, inst)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RecurrenceID.Before(out[j].RecurrenceID) })
	return out, nil
}

// inWindow reports whether t falls in [r.Start, r.End] inclusive.
func inWindow(t time.Time, r calendar.Range) bool {
	return !t.Before(r.Start) && !t.After(r.End)
}

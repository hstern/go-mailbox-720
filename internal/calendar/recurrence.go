package calendar

import (
	"fmt"
	"strings"

	goical "github.com/emersion/go-ical"
	"github.com/hstern/go-jscalendar"
	ical "github.com/hstern/go-jscalendar/ical"
)

// This file bridges between JSCalendar's structured recurrence rules — the
// lossless neutral form the calendar subsystem now carries — and the RFC 5545
// RRULE string that the Microsoft Graph frontend and iCalendar backends speak.
// Centralized here (rather than re-derived per adapter) so the conversion stays
// consistent; it wraps the go-jscalendar/ical bridge.

// RRULEFromRules renders structured JSCalendar recurrence rules to a single RFC
// 5545 RRULE property value (no "RRULE:" prefix) via the ical bridge. start and
// tz anchor the DTSTART the bridge needs to emit BY* parts correctly.
//
// Lossy collapse: RFC 8984 allows multiple rules per event but RFC 5545 / go-ical
// surface only the first RRULE, so when len(rules) > 1 only the first survives.
// This is acceptable for the Graph frontend (its patternedRecurrence cannot
// express multiple rules either); the structured rules remain intact in the
// neutral model for backends that round-trip them (JMAP, CalDAV).
func RRULEFromRules(start *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId, rules []jscalendar.RecurrenceRule) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	tmp := &jscalendar.Event{UID: "x", Start: start, TimeZone: tz, RecurrenceRules: rules}
	cal, err := ical.ToICal(tmp)
	if err != nil {
		return "", fmt.Errorf("calendar: rrule encode: %w", err)
	}
	for _, comp := range cal.Children {
		if comp.Name != goical.CompEvent {
			continue
		}
		if p := comp.Props.Get(goical.PropRecurrenceRule); p != nil {
			return p.Value, nil
		}
	}
	return "", nil
}

// RulesFromRRULE parses an RFC 5545 RRULE property value (no "RRULE:" prefix)
// into structured JSCalendar recurrence rules via the ical bridge, by decoding a
// minimal VEVENT that carries the rule.
func RulesFromRRULE(rrule string) ([]jscalendar.RecurrenceRule, error) {
	if rrule == "" {
		return nil, nil
	}
	ics := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//go-mailbox-720//EN\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:x\r\n" +
		"DTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260101T000000Z\r\n" +
		"RRULE:" + rrule + "\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	cal, err := goical.NewDecoder(strings.NewReader(ics)).Decode()
	if err != nil {
		return nil, fmt.Errorf("calendar: RulesFromRRULE decode: %w", err)
	}
	objs, err := ical.FromICal(cal)
	if err != nil {
		return nil, fmt.Errorf("calendar: RulesFromRRULE convert: %w", err)
	}
	if len(objs) == 0 {
		return nil, nil
	}
	ev, ok := objs[0].(*jscalendar.Event)
	if !ok {
		return nil, fmt.Errorf("calendar: RulesFromRRULE: unexpected object type %T", objs[0])
	}
	return ev.RecurrenceRules, nil
}

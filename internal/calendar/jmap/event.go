package jmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	goical "github.com/emersion/go-ical"
	"github.com/hstern/go-jscalendar"
	ical "github.com/hstern/go-jscalendar/ical"
	jscal "github.com/hstern/go-jscalendar/jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// toCalendarEvent maps a JMAP CalendarEvent to the backend-neutral calendar.Event.
func toCalendarEvent(ce *jscal.CalendarEvent) (calendar.Event, error) {
	if ce == nil {
		return calendar.Event{}, fmt.Errorf("jmap: nil CalendarEvent")
	}

	ev := calendar.Event{
		ID: string(ce.ID),
	}

	// CalendarID: first key of CalendarIDs (sorted for determinism).
	if len(ce.CalendarIDs) > 0 {
		keys := make([]string, 0, len(ce.CalendarIDs))
		for k := range ce.CalendarIDs {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		ev.CalendarID = keys[0]
	}

	// SeriesMasterID from BaseEventID.
	if ce.BaseEventID != nil {
		ev.SeriesMasterID = string(*ce.BaseEventID)
	}

	if ce.Event == nil {
		return ev, nil
	}

	ev.UID = ce.UID
	ev.Subject = ce.Title
	ev.Status = ce.Status
	ev.Sequence = int(ce.Sequence)
	ev.IsAllDay = ce.ShowWithoutTime

	// Body from Description.
	if ce.Description != "" {
		ct := ce.DescriptionContentType
		if ct == "" {
			ct = "text"
		}
		ev.Body = calendar.Body{
			ContentType: ct,
			Content:     ce.Description,
		}
	}

	// CreatedAt from Created.
	if ce.Created != nil {
		ev.CreatedAt = ce.Created.Time()
	}

	// Start / End: prefer UTCStart/UTCEnd; else resolve from Start+TimeZone+Duration.
	switch {
	case ce.UTCStart != nil:
		ev.Start = ce.UTCStart.Time()
		if ce.UTCEnd != nil {
			ev.End = ce.UTCEnd.Time()
		} else if ce.Duration != nil {
			ev.End = addDuration(ev.Start, ce.Duration)
		}
	case ce.Start != nil:
		ev.Start = localToUTC(ce.Start, ce.TimeZone)
		if ce.UTCEnd != nil {
			ev.End = ce.UTCEnd.Time()
		} else if ce.Duration != nil {
			ev.End = addDuration(ev.Start, ce.Duration)
		}
	}

	// RecurrenceID: resolve the override's original start to UTC.
	if ce.RecurrenceID != nil {
		ev.RecurrenceID = localToUTC(ce.RecurrenceID, ce.RecurrenceIDTimeZone)
		ev.IsOverride = true
	}

	// Location: pick deterministically (lowest Id key).
	if len(ce.Locations) > 0 {
		locKeys := make([]string, 0, len(ce.Locations))
		for k := range ce.Locations {
			locKeys = append(locKeys, string(k))
		}
		sort.Strings(locKeys)
		ev.Location = ce.Locations[jscalendar.Id(locKeys[0])].Name
	}

	// Participants: role "owner" → Organizer; role "attendee" → Attendees.
	// A participant may hold both roles (owner+attendee); owner-only participants
	// are mapped to Organizer only and are NOT added to Attendees.
	// Keys are sorted for deterministic output (ce.Participants is a map).
	if len(ce.Participants) > 0 {
		partKeys := make([]string, 0, len(ce.Participants))
		for k := range ce.Participants {
			partKeys = append(partKeys, string(k))
		}
		sort.Strings(partKeys)
		for _, k := range partKeys {
			p := ce.Participants[jscalendar.Id(k)]
			email := participantEmail(p)
			if p.Roles["owner"] {
				ev.Organizer = calendar.Address{Name: p.Name, Email: email}
			}
			if p.Roles["attendee"] {
				ev.Attendees = append(ev.Attendees, calendar.Attendee{
					Name:   p.Name,
					Email:  email,
					Status: partStatToStatus(p.ParticipationStatus),
				})
			}
		}
	}

	// Recurrence: only when RecurrenceRules are present.
	if len(ce.RecurrenceRules) > 0 {
		rrule, err := rruleFromRules(ce.Start, ce.TimeZone, ce.RecurrenceRules)
		if err != nil {
			return calendar.Event{}, err
		}
		rp := &calendar.RecurrencePattern{RRULE: rrule}

		// ExceptionDates: override keys whose patch has excluded:true.
		for key, patch := range ce.RecurrenceOverrides {
			if isExcluded(patch) {
				ldt, err := jscalendar.ParseLocalDateTime(key)
				if err != nil {
					continue // skip malformed keys
				}
				rp.ExceptionDates = append(rp.ExceptionDates, localToUTC(&ldt, ce.TimeZone))
			}
		}
		sort.Slice(rp.ExceptionDates, func(i, j int) bool {
			return rp.ExceptionDates[i].Before(rp.ExceptionDates[j])
		})

		ev.Recurrence = rp
	}

	return ev, nil
}

// rruleFromRules converts structured JSCalendar recurrence rules to a single RFC 5545
// RRULE string by routing through the ical bridge.
//
// Lossy collapse: RFC 8984 allows multiple recurrence rules per event, but RFC
// 5545 iCalendar discourages it and go-ical's Props.Get returns only the first
// RRULE property. When len(rules) > 1, only the first rule survives; the rest
// are silently dropped. Real-world JMAP events rarely carry more than one rule,
// so this is an acceptable trade-off for now.
func rruleFromRules(start *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId, rules []jscalendar.RecurrenceRule) (string, error) {
	tmp := &jscalendar.Event{UID: "x", Start: start, TimeZone: tz, RecurrenceRules: rules}
	cal, err := ical.ToICal(tmp)
	if err != nil {
		return "", fmt.Errorf("jmap: rrule encode: %w", err)
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

// partStatToStatus maps a JSCalendar participationStatus to the neutral
// calendar.Attendee.Status vocabulary, mirroring caldav/partstat.go.
func partStatToStatus(s string) string {
	switch s {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentative":
		return "tentativelyAccepted"
	case "needs-action", "":
		return "notResponded"
	default:
		return "needs-action"
	}
}

// localToUTC converts a JSCalendar LocalDateTime in the given time zone to a
// UTC time.Time. When tz is empty the time is treated as floating and the
// wall-clock fields are returned directly as UTC.
func localToUTC(ldt *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId) time.Time {
	if ldt == nil {
		return time.Time{}
	}
	if tz == "" {
		return time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
			ldt.Hour, ldt.Minute, ldt.Second, 0, time.UTC)
	}
	loc, err := time.LoadLocation(string(tz))
	if err != nil {
		// Fall back to UTC if the zone is unrecognized.
		return time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
			ldt.Hour, ldt.Minute, ldt.Second, 0, time.UTC)
	}
	return time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
		ldt.Hour, ldt.Minute, ldt.Second, 0, loc).UTC()
}

// addDuration adds a JSCalendar Duration to a time.Time.
// Calendar units (years, months) are added via time.AddDate; sub-day units
// via time.Add, matching iCalendar DURATION semantics. Each sub-day unit is
// cast individually to avoid uint64→int64 overflow from accumulated seconds.
//
// Overflow guard: time.Duration is int64 nanoseconds; its max is ~292 years.
// A valid JMAP duration for a calendar event will never approach that bound
// (RFC 8984 durations are expressed in human calendar terms), but an adversarial
// wire value could. We clamp each uint64 sub-day component to maxSubDayUnit
// (1<<31-1) so that even a malformed wire value produces a large-but-finite
// offset rather than a silently-negative wrap. d.Nanos is uint32 and is safe
// to cast directly (max ~4e9 ns < int64 max).
const maxSubDayUnit = uint64(1<<31 - 1)

func clampSubDay(v uint64) time.Duration {
	if v > maxSubDayUnit {
		v = maxSubDayUnit
	}
	return time.Duration(v)
}

func addDuration(t time.Time, d *jscalendar.Duration) time.Time {
	if d == nil {
		return t
	}
	// Calendar units first.
	t = t.AddDate(int(d.Years), int(d.Months), int(d.Days+d.Weeks*7))
	// Sub-day units: add each component separately; clamp uint64 fields to guard overflow.
	t = t.Add(clampSubDay(d.Hours)*time.Hour +
		clampSubDay(d.Minutes)*time.Minute +
		clampSubDay(d.Seconds)*time.Second +
		time.Duration(d.Nanos))
	return t
}

// participantEmail returns the best email address for a participant: the
// "imip" SendTo URI stripped of its "mailto:" prefix, falling back to Email.
func participantEmail(p jscalendar.Participant) string {
	if uri, ok := p.SendTo["imip"]; ok {
		if len(uri) > len("mailto:") && uri[:7] == "mailto:" {
			return uri[7:]
		}
		return uri
	}
	return p.Email
}

// isExcluded reports whether a PatchObject is the JSCalendar excluded:true
// deletion patch (RFC 8984, Section 4.3.5): its "excluded" member is the
// literal JSON true (not a null removal sentinel). TrimSpace guards against
// any insignificant whitespace the decoder may have preserved.
func isExcluded(patch jscalendar.PatchObject) bool {
	v, ok := patch["excluded"]
	if !ok {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(json.RawMessage(v)), []byte("true"))
}

// statusToPartStat maps the neutral calendar.Attendee.Status vocabulary
// to a JSCalendar participationStatus, inverting partStatToStatus.
func statusToPartStat(s string) string {
	switch s {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentativelyAccepted":
		return "tentative"
	case "notResponded", "":
		return "needs-action"
	default:
		return ""
	}
}

// fromCalendarEvent maps a backend-neutral calendar.Event to a JMAP
// CalendarEvent, inverting toCalendarEvent row-for-row.
func fromCalendarEvent(e calendar.Event) (*jscal.CalendarEvent, error) {
	ev := &jscalendar.Event{
		UID:      e.UID,
		Title:    e.Subject,
		Status:   e.Status,
		Sequence: uint(e.Sequence),
	}

	// ShowWithoutTime from IsAllDay.
	ev.ShowWithoutTime = e.IsAllDay

	// Description from Body.
	if e.Body.Content != "" {
		ev.Description = e.Body.Content
		if e.Body.ContentType != "" && e.Body.ContentType != "text" {
			ev.DescriptionContentType = e.Body.ContentType
		}
	}

	// Created from CreatedAt.
	if !e.CreatedAt.IsZero() {
		udt := jscalendar.UTCDateTimeFromTime(e.CreatedAt)
		ev.Created = &udt
	}

	// Start / End: emit as UTC LocalDateTime with "Etc/UTC" zone, deriving
	// Duration from End-Start. We only set Start/Duration when Start is non-zero.
	if !e.Start.IsZero() {
		ldt := jscalendar.LocalDateTime{
			Year:   e.Start.Year(),
			Month:  int(e.Start.Month()),
			Day:    e.Start.Day(),
			Hour:   e.Start.Hour(),
			Minute: e.Start.Minute(),
			Second: e.Start.Second(),
		}
		ev.Start = &ldt
		ev.TimeZone = "Etc/UTC"

		if !e.End.IsZero() {
			d := e.End.Sub(e.Start)
			dur := jscalendar.Duration{
				Hours:   uint64(d / time.Hour),
				Minutes: uint64((d % time.Hour) / time.Minute),
				Seconds: uint64((d % time.Minute) / time.Second),
			}
			ev.Duration = &dur
		}
	}

	// Location: emit as a single Locations entry keyed "1".
	if e.Location != "" {
		ev.Locations = map[jscalendar.Id]jscalendar.Location{
			"1": {Name: e.Location},
		}
	}

	// Participants: Organizer → role "owner"; Attendees → role "attendee".
	// Keys are "organizer" and "a<N>" for deterministic output.
	if e.Organizer.Email != "" || len(e.Attendees) > 0 {
		parts := make(map[jscalendar.Id]jscalendar.Participant)

		if e.Organizer.Email != "" {
			parts["organizer"] = jscalendar.Participant{
				Name:   e.Organizer.Name,
				Email:  e.Organizer.Email,
				SendTo: map[string]string{"imip": "mailto:" + e.Organizer.Email},
				Roles:  map[string]bool{"owner": true},
			}
		}
		for i, a := range e.Attendees {
			key := jscalendar.Id(fmt.Sprintf("a%d", i+1))
			parts[key] = jscalendar.Participant{
				Name:                a.Name,
				Email:               a.Email,
				SendTo:              map[string]string{"imip": "mailto:" + a.Email},
				Roles:               map[string]bool{"attendee": true},
				ParticipationStatus: statusToPartStat(a.Status),
			}
		}
		ev.Participants = parts
	}

	// Recurrence: RRULE string → structured RecurrenceRules; ExceptionDates → RecurrenceOverrides.
	if e.Recurrence != nil && e.Recurrence.RRULE != "" {
		rules, err := rulesFromRRULE(e.Recurrence.RRULE)
		if err != nil {
			return nil, err
		}
		ev.RecurrenceRules = rules

		if len(e.Recurrence.ExceptionDates) > 0 {
			overrides := make(map[string]jscalendar.PatchObject, len(e.Recurrence.ExceptionDates))
			for _, exd := range e.Recurrence.ExceptionDates {
				utcExd := exd.UTC()
				ldt := jscalendar.LocalDateTime{
					Year:   utcExd.Year(),
					Month:  int(utcExd.Month()),
					Day:    utcExd.Day(),
					Hour:   utcExd.Hour(),
					Minute: utcExd.Minute(),
					Second: utcExd.Second(),
				}
				overrides[ldt.String()] = jscalendar.PatchObject{
					"excluded": json.RawMessage("true"),
				}
			}
			ev.RecurrenceOverrides = overrides
		}
	}

	// Wrap in CalendarEvent.
	ce := jscal.FromEvent(ev)
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

// rulesFromRRULE parses an RFC 5545 RRULE property value string into a slice of
// structured JSCalendar RecurrenceRules. It does this by round-tripping through
// the ical bridge: it builds a throwaway VCALENDAR containing a VEVENT with the
// RRULE property (and a minimal DTSTART so the parser accepts it), then calls
// ical.FromICal to obtain a *jscalendar.Event and reads its RecurrenceRules.
//
// API deviation note: rather than constructing a goical.Calendar programmatically
// (which would require setting mandatory PRODID/VERSION/UID/DTSTAMP properties),
// we render a minimal ICS text string and decode it with goical.NewDecoder. This
// matches how rruleFromRules round-trips in the opposite direction.
func rulesFromRRULE(rrule string) ([]jscalendar.RecurrenceRule, error) {
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
		return nil, fmt.Errorf("jmap: rulesFromRRULE decode: %w", err)
	}
	objs, err := ical.FromICal(cal)
	if err != nil {
		return nil, fmt.Errorf("jmap: rulesFromRRULE convert: %w", err)
	}
	if len(objs) == 0 {
		return nil, nil
	}
	ev, ok := objs[0].(*jscalendar.Event)
	if !ok {
		return nil, fmt.Errorf("jmap: rulesFromRRULE: unexpected object type %T", objs[0])
	}
	return ev.RecurrenceRules, nil
}

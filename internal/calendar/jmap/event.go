package jmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
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

	// Participants: role "owner" → Organizer; all → Attendees.
	if len(ce.Participants) > 0 {
		for _, p := range ce.Participants {
			email := participantEmail(p)
			if p.Roles["owner"] {
				ev.Organizer = calendar.Address{Name: p.Name, Email: email}
			}
			ev.Attendees = append(ev.Attendees, calendar.Attendee{
				Name:   p.Name,
				Email:  email,
				Status: partStatToStatus(p.ParticipationStatus),
			})
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
		return ""
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
func addDuration(t time.Time, d *jscalendar.Duration) time.Time {
	if d == nil {
		return t
	}
	// Calendar units first.
	t = t.AddDate(int(d.Years), int(d.Months), int(d.Days+d.Weeks*7))
	// Sub-day units: add each component separately to keep each cast in range.
	t = t.Add(time.Duration(d.Hours)*time.Hour +
		time.Duration(d.Minutes)*time.Minute +
		time.Duration(d.Seconds)*time.Second +
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

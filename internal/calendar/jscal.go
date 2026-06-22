package calendar

import (
	"fmt"
	"slices"
	"time"

	"github.com/hstern/go-jscalendar"
)

// This file holds the shared JSCalendar (RFC 8984) helper contract the whole
// calendar subsystem maps through: time construction/emission that preserves the
// event's named time zone (the fidelity the bespoke UTC-only pivot lost), and
// participant access/construction that bridges JSCalendar's role-keyed
// Participants map to the organizer/attendee + Graph responseStatus vocabulary.
// Adapters (CalDAV/JMAP) and the server Graph layer all use these so the mapping
// stays consistent in one place rather than re-hand-rolled per package.

// --- Time construction (UTC instants → JSCalendar) ---

// SetUTCTimes sets Start as a UTC wall-clock LocalDateTime (TimeZone "Etc/UTC")
// and Duration from a pair of UTC instants. It is the construction path for code
// that works in UTC instants (the scheduling engine, fakes). A zero start leaves
// Start/TimeZone/Duration untouched.
func (e *Event) SetUTCTimes(start, end time.Time) {
	if start.IsZero() {
		return
	}
	start = start.UTC()
	ldt := localDateTimeFromTime(start)
	e.Start = &ldt
	e.TimeZone = "Etc/UTC"
	if !end.IsZero() {
		e.Duration = durationFromGo(end.UTC().Sub(start))
	}
}

func localDateTimeFromTime(t time.Time) jscalendar.LocalDateTime {
	return jscalendar.LocalDateTime{
		Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
	}
}

// durationFromGo expresses a Go duration as a JSCalendar Duration in hours/
// minutes/seconds (matching iCalendar DURATION emission). Negative durations
// clamp to zero.
func durationFromGo(d time.Duration) *jscalendar.Duration {
	if d <= 0 {
		return nil
	}
	return &jscalendar.Duration{
		Hours:   uint64(d / time.Hour),
		Minutes: uint64((d % time.Hour) / time.Minute),
		Seconds: uint64((d % time.Minute) / time.Second),
	}
}

// --- Graph dateTimeTimeZone emission (tz-preserving) ---

// graphTimeFormat is Graph's dateTimeTimeZone.dateTime layout (7 fractional
// digits, no zone — the zone travels in the sibling timeZone field).
const graphTimeFormat = "2006-01-02T15:04:05.0000000"

// StartGraph returns the Start as a Graph dateTimeTimeZone pair (dateTime string
// + IANA zone name), preserving the event's TimeZone rather than collapsing to
// UTC. A floating Start (no TimeZone) reports "UTC". ok is false when Start is
// unset.
func (e Event) StartGraph() (dateTime, timeZone string, ok bool) {
	if e.Start == nil {
		return "", "", false
	}
	return formatGraphLDT(*e.Start), graphZoneName(e.TimeZone), true
}

// EndGraph returns the event's end as a Graph dateTimeTimeZone pair in the same
// zone as Start (end = Start + Duration). ok is false when Start is unset.
func (e Event) EndGraph() (dateTime, timeZone string, ok bool) {
	if e.Start == nil {
		return "", "", false
	}
	loc := time.UTC
	if e.TimeZone != "" {
		if l, err := time.LoadLocation(string(e.TimeZone)); err == nil {
			loc = l
		}
	}
	endLocal := e.EndTime().In(loc)
	return endLocal.Format(graphTimeFormat), graphZoneName(e.TimeZone), true
}

func formatGraphLDT(l jscalendar.LocalDateTime) string {
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.0000000",
		l.Year, l.Month, l.Day, l.Hour, l.Minute, l.Second)
}

func graphZoneName(tz jscalendar.TimeZoneId) string {
	if tz == "" {
		return "UTC"
	}
	return string(tz)
}

// SetStartGraph sets Start (parsed from a Graph dateTime string) and TimeZone
// (the Graph timeZone name; "" and "UTC" both map to "Etc/UTC"). It is the
// inverse of StartGraph for the server write path.
func (e *Event) SetStartGraph(dateTime, timeZone string) error {
	ldt, err := parseGraphDateTime(dateTime)
	if err != nil {
		return err
	}
	e.Start = &ldt
	switch timeZone {
	case "", "UTC":
		e.TimeZone = "Etc/UTC"
	default:
		e.TimeZone = jscalendar.TimeZoneId(timeZone)
	}
	return nil
}

// SetEndGraph sets Duration from a Graph end dateTime (interpreted in the event's
// already-set TimeZone) relative to Start. Start must be set first.
func (e *Event) SetEndGraph(dateTime string) error {
	if e.Start == nil {
		return fmt.Errorf("calendar: SetEndGraph before Start")
	}
	endLDT, err := parseGraphDateTime(dateTime)
	if err != nil {
		return err
	}
	end := localToUTC(&endLDT, e.TimeZone)
	e.Duration = durationFromGo(end.Sub(e.StartTime()))
	return nil
}

// parseGraphDateTime parses a Graph dateTimeTimeZone.dateTime value into a
// JSCalendar LocalDateTime. Graph emits 7 fractional digits but clients may send
// fewer (or none); we accept the common layouts.
func parseGraphDateTime(s string) (jscalendar.LocalDateTime, error) {
	for _, layout := range []string{graphTimeFormat, "2006-01-02T15:04:05", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return localDateTimeFromTime(t), nil
		}
	}
	return jscalendar.LocalDateTime{}, fmt.Errorf("calendar: unparseable Graph dateTime %q", s)
}

// --- Participants ---

// Organizer returns the participant holding the "owner" role, if any.
func (e Event) Organizer() (jscalendar.Participant, bool) {
	for _, k := range sortedParticipantKeys(e.Participants) {
		if p := e.Participants[k]; p.Roles["owner"] {
			return p, true
		}
	}
	return jscalendar.Participant{}, false
}

// Attendees returns the participants holding the "attendee" role, in
// deterministic (sorted-key) order.
func (e Event) Attendees() []jscalendar.Participant {
	var out []jscalendar.Participant
	for _, k := range sortedParticipantKeys(e.Participants) {
		if p := e.Participants[k]; p.Roles["attendee"] {
			out = append(out, p)
		}
	}
	return out
}

func sortedParticipantKeys(m map[jscalendar.Id]jscalendar.Participant) []jscalendar.Id {
	keys := make([]jscalendar.Id, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ParticipantEmail returns a participant's email: the "imip" sendTo URI stripped
// of its "mailto:" prefix, falling back to the Email field.
func ParticipantEmail(p jscalendar.Participant) string {
	if uri, ok := p.SendTo["imip"]; ok {
		if len(uri) > len("mailto:") && uri[:len("mailto:")] == "mailto:" {
			return uri[len("mailto:"):]
		}
		return uri
	}
	return p.Email
}

// NewParticipant builds a JSCalendar participant for the given roles, wiring the
// imip sendTo URI from the email and (for attendees) the participation status.
// partStat is a JSCalendar participationStatus ("accepted"/"declined"/
// "tentative"/"needs-action"); pass "" to omit it.
func NewParticipant(name, email, partStat string, roles ...string) jscalendar.Participant {
	p := jscalendar.Participant{
		Name:                name,
		Email:               email,
		ParticipationStatus: partStat,
		Roles:               map[string]bool{},
	}
	if email != "" {
		p.SendTo = map[string]string{"imip": "mailto:" + email}
	}
	for _, r := range roles {
		p.Roles[r] = true
	}
	return p
}

// SetOrganizerAttendees populates Participants from an optional organizer and a
// list of attendees, keying "organizer" and "a1".."aN" for deterministic output.
// Both nil leaves Participants unset.
func (e *Event) SetOrganizerAttendees(organizer *jscalendar.Participant, attendees []jscalendar.Participant) {
	if organizer == nil && len(attendees) == 0 {
		return
	}
	parts := make(map[jscalendar.Id]jscalendar.Participant)
	if organizer != nil {
		parts["organizer"] = *organizer
	}
	for i, a := range attendees {
		parts[jscalendar.Id(fmt.Sprintf("a%d", i+1))] = a
	}
	e.Participants = parts
}

// --- Participation status ↔ Graph responseType ---

// PartStatToResponse maps a JSCalendar participationStatus to Microsoft Graph's
// responseStatus.response vocabulary.
func PartStatToResponse(partStat string) string {
	switch partStat {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentative":
		return "tentativelyAccepted"
	case "needs-action", "":
		return "notResponded"
	default:
		return "notResponded"
	}
}

// ResponseToPartStat maps a Microsoft Graph responseStatus.response value to a
// JSCalendar participationStatus, inverting PartStatToResponse.
func ResponseToPartStat(response string) string {
	switch response {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentativelyAccepted":
		return "tentative"
	case "notResponded", "none", "":
		return "needs-action"
	default:
		return "needs-action"
	}
}

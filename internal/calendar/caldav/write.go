package caldav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/hstern/go-jscalendar"
	jsical "github.com/hstern/go-jscalendar/ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

var _ calendar.Writer = (*Client)(nil)

// productID identifies this adapter in the VCALENDAR objects it writes (the
// PRODID property required by RFC 5545). It mirrors the scheduling engine's
// PRODID convention.
const productID = "-//go-mailbox-720//caldav//EN"

// CreateEvent builds a VCALENDAR/VEVENT from e and PUTs it as a new calendar
// object resource under the named calendar collection. It mints a fresh
// iCalendar UID (unless e.UID is already set) and an object path of the form
// "<collection>/<uid>.ics", then returns the event stamped with the opaque ID
// that encodes that path (and its UID and CalendarID).
func (cl *Client) CreateEvent(ctx context.Context, calID string, e calendar.Event) (calendar.Event, error) {
	calPath, err := decodeCalendarID(calID)
	if err != nil {
		return calendar.Event{}, err
	}
	if e.UID == "" {
		e.UID = newUID()
	}
	name, err := eventObjectName(e.UID)
	if err != nil {
		return calendar.Event{}, err
	}
	objectPath := path.Join(calPath, name)
	// path.Join strips a trailing slash; CalDAV object resources are addressed
	// by their full href, which has no trailing slash, so this is correct.
	if err := cl.putEvent(ctx, objectPath, e); err != nil {
		return calendar.Event{}, fmt.Errorf("caldav: create event in %q: %w", calPath, err)
	}
	e.ID = eventID(objectPath)
	e.CalendarID = calID
	return e, nil
}

// UpdateEvent overwrites the calendar object resource identified by e.ID with a
// VCALENDAR/VEVENT built from e. The opaque ID decodes to the object path; the
// event keeps its existing UID, so callers should preserve e.UID across a
// read-modify-write.
func (cl *Client) UpdateEvent(ctx context.Context, e calendar.Event) (calendar.Event, error) {
	objectPath, err := decodeEventID(e.ID)
	if err != nil {
		return calendar.Event{}, err
	}
	if err := cl.putEvent(ctx, objectPath, e); err != nil {
		return calendar.Event{}, fmt.Errorf("caldav: update event %q: %w", objectPath, err)
	}
	e.CalendarID = calendarIDForObject(objectPath)
	return e, nil
}

// DeleteEvent removes the calendar object resource identified by id via an
// authenticated HTTP DELETE (go-webdav's RemoveAll). Deleting a resource that no
// longer exists returns the server's error (typically a 404) — matching Graph's
// own DELETE semantics; a caller wanting idempotent cleanup can ignore a
// not-found error.
func (cl *Client) DeleteEvent(ctx context.Context, id string) error {
	objectPath, err := decodeEventID(id)
	if err != nil {
		return err
	}
	if err := cl.c.RemoveAll(ctx, objectPath); err != nil {
		return fmt.Errorf("caldav: delete event %q: %w", objectPath, err)
	}
	return nil
}

// putEvent encodes e as a single-VEVENT VCALENDAR and PUTs it at objectPath.
func (cl *Client) putEvent(ctx context.Context, objectPath string, e calendar.Event) error {
	cal := eventToICal(e)
	if _, err := cl.c.PutCalendarObject(ctx, objectPath, cal); err != nil {
		return fmt.Errorf("put calendar object: %w", err)
	}
	return nil
}

var _ calendar.InstanceWriter = (*Client)(nil)

// WriteInstanceOverride records a per-occurrence override of a recurring series:
// it adds (or replaces) an override VEVENT carrying the occurrence's RECURRENCE-ID
// inside the series object resource, leaving the master and the other overrides
// intact. masterID is the opaque series-master ID; override.RecurrenceID names the
// occurrence to override (it must be non-zero). It returns the stored override
// event stamped with its instance ID.
//
// It implements calendar.InstanceWriter via a read-modify-write on the single
// object resource that holds the whole series (master + overrides share one UID),
// since CalDAV addresses a series by that one resource rather than per instance.
func (cl *Client) WriteInstanceOverride(ctx context.Context, masterID string, override calendar.Event) (calendar.Event, error) {
	rid := recurrenceIDTime(override)
	if rid.IsZero() {
		return calendar.Event{}, fmt.Errorf("caldav: WriteInstanceOverride requires a RecurrenceID")
	}
	// Decode the series-master (or instance) id to the object path; both forms
	// address the one resource holding the whole series, so the recurrence-id half
	// of an instance id is discarded here.
	objectPath, _, _, err := decodeInstanceEventID(masterID)
	if err != nil {
		return calendar.Event{}, err
	}
	obj, err := cl.c.GetCalendarObject(ctx, objectPath)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("caldav: get calendar object %q: %w", objectPath, err)
	}
	cal := obj.Data
	if cal == nil {
		return calendar.Event{}, fmt.Errorf("caldav: object %q has no calendar data", objectPath)
	}

	uid := seriesUID(cal)
	override.UID = uid
	overrideEvent := overrideVEVENT(override)

	// Replace any existing override for this RECURRENCE-ID; otherwise append.
	children := cal.Children[:0]
	for _, child := range cal.Children {
		if child.Name == ical.CompEvent {
			ev := &ical.Event{Component: child}
			if existing, ok := recurrenceIDOf(ev); ok && existing.Equal(rid) {
				continue // drop the stale override; the fresh one is appended below
			}
		}
		children = append(children, child)
	}
	cal.Children = append(children, overrideEvent.Component)

	if _, err := cl.c.PutCalendarObject(ctx, objectPath, cal); err != nil {
		return calendar.Event{}, fmt.Errorf("caldav: put override into %q: %w", objectPath, err)
	}

	override.ID = instanceEventID(objectPath, rid)
	override.CalendarID = calendarIDForObject(objectPath)
	override.SeriesMasterID = eventID(objectPath)
	override.IsOverride = true
	return override, nil
}

// seriesUID returns the UID shared by the VEVENTs of a series object (the master's
// UID), so a freshly built override VEVENT carries the matching UID. Returns "" if
// no VEVENT carries one.
func seriesUID(cal *ical.Calendar) string {
	for _, ev := range cal.Events() {
		if uid := propText(&ev, ical.PropUID); uid != "" {
			return uid
		}
	}
	return ""
}

// overrideVEVENT builds the override VEVENT for a single occurrence: the event's
// fields plus its RECURRENCE-ID. It reuses eventToICal and lifts out the single
// VEVENT, since eventToICal already emits the RECURRENCE-ID for an event with a
// non-zero RecurrenceID.
func overrideVEVENT(e calendar.Event) *ical.Event {
	e.RecurrenceRules = nil // an override is one occurrence, never a nested series
	cal := eventToICal(e)
	events := cal.Events()
	return &events[0]
}

// eventToICal builds a VCALENDAR holding a single VEVENT from a neutral Event,
// the inverse of mapEvent. It routes the embedded jscalendar.Event through the
// go-jscalendar/ical bridge — the single iCal↔JSCalendar mapping — and then layers
// on the three CalDAV-relevant bits the bridge does not carry: each attendee's
// PARTSTAT, the override RECURRENCE-ID, and the master's EXDATE exceptions. A
// calendar object resource stored in a collection carries no METHOD property (that
// is reserved for iTIP scheduling objects), and go-ical's encoder emits the
// RFC 5545-required CRLF line endings.
func eventToICal(e calendar.Event) *ical.Calendar {
	// Sanitize participant display names before the bridge writes them as CN
	// parameters: go-ical (and the bridge) do not escape parameter values, so an
	// unescaped CR/LF in a name could inject forged property lines.
	src := sanitizeParticipantNames(e.Event)

	cal, err := jsical.ToICal(&src)
	if err != nil || len(cal.Events()) == 0 {
		// Degrade to a minimal valid object rather than panic on a malformed event;
		// callers PUT the result, and an empty VEVENT is still a valid resource.
		cal = ical.NewCalendar()
		cal.Props.SetText(ical.PropVersion, "2.0")
		ev := ical.NewEvent()
		ev.Props.SetText(ical.PropUID, e.UID)
		ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
		cal.Children = append(cal.Children, ev.Component)
	}
	// Stamp the adapter's own PRODID over the bridge's, preserving the prior
	// identity of objects this adapter writes.
	cal.Props.SetText(ical.PropProductID, productID)

	events := cal.Events()
	ev := &events[0]

	// PARTSTAT: the bridge writes ORGANIZER/ATTENDEE but drops participationStatus.
	// Re-apply each attendee's PARTSTAT by matching the CAL-ADDRESS.
	applyPartStat(ev, e)

	// EXDATE: the bridge does not emit per-date recurrence exceptions, so a master's
	// EXDATEs are derived from the recurrence-override map entries marked excluded.
	for _, ex := range excludedDates(e) {
		exdate := ical.NewProp(ical.PropExceptionDates)
		exdate.SetDateTime(ex.UTC())
		ev.Props.Add(exdate)
	}

	// RECURRENCE-ID: the bridge does not emit the override pointer, so an override
	// instance (non-nil envelope RecurrenceID) carries it explicitly.
	if rid := recurrenceIDTime(e); !rid.IsZero() {
		ridProp := ical.NewProp(ical.PropRecurrenceID)
		ridProp.SetDateTime(rid.UTC())
		ev.Props.Set(ridProp)
	}

	return cal
}

// sanitizeParticipantNames returns a shallow copy of the event whose participants'
// display names are stripped of characters unsafe in an iCalendar parameter value
// (the bridge does not escape CN parameters).
func sanitizeParticipantNames(e jscalendar.Event) jscalendar.Event {
	if len(e.Participants) == 0 {
		return e
	}
	parts := make(map[jscalendar.Id]jscalendar.Participant, len(e.Participants))
	for id, p := range e.Participants {
		p.Name = sanitizeParam(p.Name)
		parts[id] = p
	}
	e.Participants = parts
	return e
}

// applyPartStat sets the PARTSTAT and RFC 6638 SCHEDULE-STATUS parameters on each
// ATTENDEE property the bridge emitted, looking up the matching attendee
// participant by scheduling address. SCHEDULE-STATUS carries the client-side
// scheduling delivery outcome the server records onto the participant.
func applyPartStat(ev *ical.Event, e calendar.Event) {
	type attendeeAttrs struct{ partStat, schedStatus string }
	byAddr := make(map[string]attendeeAttrs)
	for _, p := range e.Attendees() {
		a := attendeeAttrs{partStat: jsCalToPartStat[p.ParticipationStatus]}
		if len(p.ScheduleStatus) > 0 {
			a.schedStatus = p.ScheduleStatus[0]
		}
		if a.partStat == "" && a.schedStatus == "" {
			continue
		}
		addr := p.SendTo["imip"]
		if addr == "" && p.Email != "" {
			addr = "mailto:" + p.Email
		}
		byAddr[normalizeCalAddress(addr)] = a
	}
	if len(byAddr) == 0 {
		return
	}
	for i := range ev.Props[ical.PropAttendee] {
		att := &ev.Props[ical.PropAttendee][i]
		a, ok := byAddr[normalizeCalAddress(att.Value)]
		if !ok {
			continue
		}
		if a.partStat != "" {
			att.Params.Set(ical.ParamParticipationStatus, a.partStat)
		}
		if a.schedStatus != "" {
			att.Params.Set(paramScheduleStatus, a.schedStatus)
		}
	}
}

// excludedDates returns the LocalDateTime keys of the event's recurrence-override
// entries that are whole-occurrence exclusions (RFC 8984 §4.3.5 "excluded":true) —
// the JSCalendar equivalent of EXDATE — as UTC instants. Returns nil when the
// event carries no overrides.
func excludedDates(e calendar.Event) []time.Time {
	var out []time.Time
	for key, patch := range e.RecurrenceOverrides {
		raw, ok := patch["excluded"]
		if !ok || strings.TrimSpace(string(raw)) != "true" {
			continue
		}
		if ldt, err := jscalendar.ParseLocalDateTime(key); err == nil {
			loc := time.UTC
			if e.TimeZone != "" {
				if l, lerr := time.LoadLocation(string(e.TimeZone)); lerr == nil {
					loc = l
				}
			}
			out = append(out, time.Date(ldt.Year, time.Month(ldt.Month), ldt.Day,
				ldt.Hour, ldt.Minute, ldt.Second, 0, loc).UTC())
		}
	}
	return out
}

// eventObjectName returns the ".ics" object filename for a UID, rejecting a
// caller-supplied UID that could escape the calendar collection: CreateEvent
// joins this onto the collection path, so a UID with a path separator or a "."/
// ".." segment would otherwise write outside the named collection.
func eventObjectName(uid string) (string, error) {
	if uid == "." || uid == ".." || strings.ContainsAny(uid, `/\`) ||
		strings.IndexFunc(uid, func(r rune) bool { return r < 0x20 }) >= 0 {
		return "", fmt.Errorf("caldav: unsafe event UID %q", uid)
	}
	return uid + ".ics", nil
}

// sanitizeParam strips characters unsafe in an iCalendar property parameter
// value. go-ical escapes TEXT property *values* but not parameter values, so
// without this a CN (display name) containing CR/LF could inject forged property
// lines into the encoded object, and a double quote could break out of a quoted
// parameter.
func sanitizeParam(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' {
			return -1
		}
		return r
	}, s)
}

// newUID mints a fresh iCalendar UID for a created event: 16 random bytes hex
// encoded, scoped with this adapter's name to make it globally unique per
// RFC 5545 §3.8.4.7. The UID doubles as the object resource name, so the scope is
// joined with '-' rather than '@': an '@' in the resource path breaks object GETs
// against servers whose calendar home is itself email-addressed (e.g. Stalwart's
// /dav/cal/user@domain/), even though '@' is a valid path character.
func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + "-go-mailbox-720"
}

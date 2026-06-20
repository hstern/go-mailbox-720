package caldav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"

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
	if override.RecurrenceID.IsZero() {
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
	rid := override.RecurrenceID.UTC()
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
	e.Recurrence = nil // an override is one occurrence, never a nested series
	cal := eventToICal(e)
	events := cal.Events()
	return &events[0]
}

// eventToICal builds a VCALENDAR holding a single VEVENT from a neutral Event,
// the inverse of mapEvent. It is the write-path counterpart used by CreateEvent
// and UpdateEvent: a calendar object resource stored in a collection carries no
// METHOD property (that is reserved for iTIP scheduling objects), and go-ical's
// encoder emits the RFC 5545-required CRLF line endings.
func eventToICal(e calendar.Event) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, productID)

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, e.UID)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	if e.Sequence > 0 {
		// A bare integer property — SetText would tag it VALUE=TEXT, contradicting
		// SEQUENCE's INTEGER type and tripping up strict CalDAV clients.
		seq := ical.NewProp(ical.PropSequence)
		seq.Value = strconv.Itoa(e.Sequence)
		ev.Props.Set(seq)
	}
	if !e.Start.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeStart, e.Start.UTC())
	}
	if !e.End.IsZero() {
		ev.Props.SetDateTime(ical.PropDateTimeEnd, e.End.UTC())
	}
	if e.Subject != "" {
		ev.Props.SetText(ical.PropSummary, e.Subject)
	}
	if e.Location != "" {
		ev.Props.SetText(ical.PropLocation, e.Location)
	}
	if e.Body.Content != "" {
		ev.Props.SetText(ical.PropDescription, e.Body.Content)
	}
	// A series master carries its recurrence rule (RRULE) and EXDATE exceptions.
	if e.Recurrence != nil && strings.TrimSpace(e.Recurrence.RRULE) != "" {
		rrule := ical.NewProp(ical.PropRecurrenceRule)
		rrule.SetValueType(ical.ValueRecurrence)
		rrule.Value = strings.TrimSpace(e.Recurrence.RRULE)
		ev.Props.Set(rrule)
		for _, ex := range e.Recurrence.ExceptionDates {
			exdate := ical.NewProp(ical.PropExceptionDates)
			exdate.SetDateTime(ex.UTC())
			ev.Props.Add(exdate)
		}
	}
	// An override (exception) instance carries a RECURRENCE-ID naming the
	// occurrence it replaces.
	if !e.RecurrenceID.IsZero() {
		rid := ical.NewProp(ical.PropRecurrenceID)
		rid.SetDateTime(e.RecurrenceID.UTC())
		ev.Props.Set(rid)
	}
	if org := buildAddress(ical.PropOrganizer, e.Organizer); org != nil {
		ev.Props.Set(org)
	}
	for _, a := range e.Attendees {
		att := buildAddress(ical.PropAttendee, calendar.Address{Name: a.Name, Email: a.Email})
		if att == nil {
			continue
		}
		if ps := statusToPartStat[a.Status]; ps != "" {
			att.Params.Set(ical.ParamParticipationStatus, ps)
		}
		if a.ScheduleStatus != "" {
			att.Params.Set(paramScheduleStatus, a.ScheduleStatus)
		}
		ev.Props.Add(att)
	}

	cal.Children = append(cal.Children, ev.Component)
	return cal
}

// buildAddress constructs a CAL-ADDRESS property (ORGANIZER or ATTENDEE) from a
// neutral Address, encoding the email as a "mailto:" URI and the name as a CN
// parameter. It is the inverse of calAddress. Returns nil when the address has
// no email.
func buildAddress(name string, addr calendar.Address) *ical.Prop {
	if addr.Email == "" {
		return nil
	}
	prop := ical.NewProp(name)
	prop.Value = "mailto:" + addr.Email
	if addr.Name != "" {
		prop.Params.Set(ical.ParamCommonName, sanitizeParam(addr.Name))
	}
	return prop
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

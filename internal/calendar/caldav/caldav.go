// Package caldav implements the calendar.Backend port against a CalDAV server
// using emersion/go-webdav (the caldav subpackage) for the protocol and
// emersion/go-ical for iCalendar parsing. A Client is bound to one authenticated
// CalDAV principal.
//
// First cut: the read paths (calendar discovery, event listing, single-event
// fetch). Deferred to their own issues (mirroring the mail port): push
// notifications, delta sync tokens, $filter execution, and event submission.
package caldav

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	gocaldav "github.com/emersion/go-webdav/caldav"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// Options configures the CalDAV connection.
type Options struct {
	// HTTPClient performs the underlying requests. When nil, http.DefaultClient
	// is used. Supply a custom client to control TLS, timeouts, or proxies.
	HTTPClient webdav.HTTPClient
}

// Client is a CalDAV-backed calendar.Backend over a single authenticated
// principal. http and endpoint are retained alongside the go-webdav client so the
// adapter can issue the one request go-webdav does not expose: an OPTIONS probe
// for the RFC 6638 calendar-auto-schedule capability (see SupportsServerScheduling).
type Client struct {
	c        *gocaldav.Client
	http     webdav.HTTPClient
	endpoint *url.URL
}

var _ calendar.Backend = (*Client)(nil)
var _ calendar.Finder = (*Client)(nil)

// Dial builds a CalDAV client for endpoint (the server's CalDAV base URL),
// authenticating every request with HTTP Basic credentials. It does not perform
// any network I/O itself — discovery happens lazily on the first call.
func Dial(endpoint, username, password string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpClient = webdav.HTTPClientWithBasicAuth(httpClient, username, password)

	c, err := gocaldav.NewClient(httpClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("caldav: new client for %s: %w", endpoint, err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("caldav: parse endpoint %s: %w", endpoint, err)
	}
	return &Client{c: c, http: httpClient, endpoint: u}, nil
}

// Close releases the backend. The CalDAV client holds no persistent connection
// of its own (it rides on net/http connection pooling), so this is a no-op.
func (cl *Client) Close() error {
	return nil
}

// ListCalendars discovers the principal's calendar collections via the CalDAV
// calendar-home-set, then PROPFINDs each calendar's metadata.
func (cl *Client) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	principal, err := cl.c.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("caldav: find principal: %w", err)
	}
	homeSet, err := cl.c.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("caldav: find calendar home set: %w", err)
	}
	cals, err := cl.c.FindCalendars(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("caldav: find calendars: %w", err)
	}
	out := make([]calendar.Calendar, 0, len(cals))
	for _, c := range cals {
		out = append(out, calendar.Calendar{
			ID:          calendarID(c.Path),
			Name:        c.Name,
			Description: c.Description,
		})
	}
	return out, nil
}

// ListEvents lists the events in a calendar via a CalDAV calendar-query REPORT,
// optionally bounded by a time range. Every returned VEVENT is mapped to an
// Event; a calendar object that fails to map is skipped rather than failing the
// whole listing.
func (cl *Client) ListEvents(ctx context.Context, calID string, r calendar.Range) ([]calendar.Event, error) {
	calPath, err := decodeCalendarID(calID)
	if err != nil {
		return nil, err
	}
	query := &gocaldav.CalendarQuery{
		CompRequest: gocaldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			Comps:    []gocaldav.CalendarCompRequest{{Name: ical.CompEvent, AllProps: true}},
		},
		CompFilter: gocaldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []gocaldav.CompFilter{{
				Name:  ical.CompEvent,
				Start: r.Start,
				End:   r.End,
			}},
		},
	}
	objs, err := cl.c.QueryCalendar(ctx, calPath, query)
	if err != nil {
		return nil, fmt.Errorf("caldav: query calendar %q: %w", calPath, err)
	}
	var events []calendar.Event
	for _, obj := range objs {
		if e, ok := eventFromObject(calID, obj.Path, obj.Data); ok {
			events = append(events, e)
		}
	}
	return events, nil
}

// FindEventByUID locates the event in a calendar whose iCalendar UID matches uid,
// via a calendar-query REPORT filtered on the VEVENT UID property, and reports
// whether one was found. It backs the inbound scheduling trigger's UID correlation
// (calendar.Finder). The text match is exact; the first matching object's master
// VEVENT is returned.
func (cl *Client) FindEventByUID(ctx context.Context, calID, uid string) (calendar.Event, bool, error) {
	calPath, err := decodeCalendarID(calID)
	if err != nil {
		return calendar.Event{}, false, err
	}
	query := &gocaldav.CalendarQuery{
		CompRequest: gocaldav.CalendarCompRequest{
			Name:     ical.CompCalendar,
			AllProps: true,
			Comps:    []gocaldav.CalendarCompRequest{{Name: ical.CompEvent, AllProps: true}},
		},
		CompFilter: gocaldav.CompFilter{
			Name: ical.CompCalendar,
			Comps: []gocaldav.CompFilter{{
				Name:  ical.CompEvent,
				Props: []gocaldav.PropFilter{{Name: ical.PropUID, TextMatch: &gocaldav.TextMatch{Text: uid}}},
			}},
		},
	}
	objs, err := cl.c.QueryCalendar(ctx, calPath, query)
	if err != nil {
		return calendar.Event{}, false, fmt.Errorf("caldav: query calendar %q by uid: %w", calPath, err)
	}
	for _, obj := range objs {
		// A server with no UID-property filtering may return every object; confirm
		// the UID rather than trusting the server narrowed it.
		if e, ok := eventFromObject(calID, obj.Path, obj.Data); ok && e.UID == uid {
			return e, true, nil
		}
	}
	return calendar.Event{}, false, nil
}

// GetEvent fetches a single event resource by opaque ID and maps it to a neutral
// Event. A plain (master) id maps the master VEVENT; an instance id — one minted
// by ListInstances that carries a RECURRENCE-ID — maps the addressed occurrence
// (its stored override VEVENT when one exists, otherwise the occurrence
// synthesized from the master's RRULE).
func (cl *Client) GetEvent(ctx context.Context, id string) (calendar.Event, error) {
	objectPath, recurrenceID, isInstance, err := decodeInstanceEventID(id)
	if err != nil {
		return calendar.Event{}, err
	}
	obj, err := cl.c.GetCalendarObject(ctx, objectPath)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("caldav: get calendar object %q: %w", objectPath, err)
	}
	calID := calendarIDForObject(objectPath)
	if isInstance {
		e, ok := instanceFromObject(calID, objectPath, obj.Data, recurrenceID)
		if !ok {
			return calendar.Event{}, fmt.Errorf("caldav: event %s has no such occurrence", id)
		}
		return e, nil
	}
	e, ok := eventFromObject(calID, objectPath, obj.Data)
	if !ok {
		return calendar.Event{}, fmt.Errorf("caldav: event %s has no VEVENT", id)
	}
	return e, nil
}

// instanceFromObject resolves one occurrence of a series — addressed by its
// RECURRENCE-ID instant — to a neutral Event. It prefers a stored override VEVENT
// for that instant; failing that it synthesizes the occurrence from the master
// (start = recurrenceID, end shifted by the master's duration). Reports false when
// the object holds no master VEVENT.
func instanceFromObject(calID, objectPath string, cal *ical.Calendar, recurrenceID time.Time) (calendar.Event, bool) {
	if cal == nil {
		return calendar.Event{}, false
	}
	events := cal.Events()
	masterIdx := -1
	for i := range events {
		if rid, ok := recurrenceIDOf(&events[i]); ok {
			if rid.Equal(recurrenceID) {
				e := mapEvent(&events[i])
				e.IsOverride = true
				e.RecurrenceID = recurrenceID
				e.ID = instanceEventID(objectPath, recurrenceID)
				e.CalendarID = calID
				e.SeriesMasterID = eventID(objectPath)
				return e, true
			}
			continue
		}
		if masterIdx < 0 {
			masterIdx = i
		}
	}
	if masterIdx < 0 {
		return calendar.Event{}, false
	}
	master := &events[masterIdx]
	e := mapEvent(master)
	e.Recurrence = nil // a single occurrence is not itself a series
	start, _ := master.DateTimeStart(time.UTC)
	end, _ := master.DateTimeEnd(time.UTC)
	e.Start = recurrenceID
	if !start.IsZero() && !end.IsZero() {
		e.End = recurrenceID.Add(end.Sub(start))
	}
	e.RecurrenceID = recurrenceID
	e.ID = instanceEventID(objectPath, recurrenceID)
	e.CalendarID = calID
	e.SeriesMasterID = eventID(objectPath)
	return e, true
}

// propRecurrenceID is the VEVENT property marking a recurrence override instance
// (go-ical exposes no constant for it).
const propRecurrenceID = "RECURRENCE-ID"

// eventFromObject maps one CalDAV calendar object to a single neutral Event,
// using the master VEVENT — the component without a RECURRENCE-ID. The VEVENTs
// in an object share one UID (a master plus recurrence overrides) and thus one
// opaque event ID, so a series is represented at the collection level by its
// master, which carries the recurrence pattern (RRULE/EXDATE). The individual
// occurrences and overrides are surfaced separately, addressed by instance IDs,
// via ListInstances / GetEvent. Reports false when the object holds no VEVENT.
func eventFromObject(calID, objectPath string, cal *ical.Calendar) (calendar.Event, bool) {
	if cal == nil {
		return calendar.Event{}, false
	}
	events := cal.Events()
	if len(events) == 0 {
		return calendar.Event{}, false
	}
	master := -1
	for i := range events {
		if events[i].Props.Get(propRecurrenceID) == nil {
			master = i
			break
		}
	}
	if master < 0 {
		// Only recurrence overrides and no master component: a malformed object we
		// cannot represent as one event (and whose SEQUENCE would mislead the
		// Finder's revision comparison). Skip it rather than treat an override as
		// the master.
		return calendar.Event{}, false
	}
	e := mapEvent(&events[master])
	e.ID = eventID(objectPath)
	e.CalendarID = calID
	return e, true
}

// mapEvent maps an iCalendar VEVENT to a neutral Event. It is best-effort: a
// missing or malformed property yields a zero value for that field rather than
// failing the whole event. ID and CalendarID are left to the caller.
func mapEvent(ev *ical.Event) calendar.Event {
	e := calendar.Event{
		Subject:   propText(ev, ical.PropSummary),
		UID:       propText(ev, ical.PropUID),
		Location:  propText(ev, ical.PropLocation),
		Status:    strings.ToLower(propText(ev, ical.PropStatus)),
		Sequence:  propInt(ev, ical.PropSequence),
		Organizer: calAddress(ev.Props.Get(ical.PropOrganizer)),
		Body: calendar.Body{
			ContentType: "text",
			Content:     propText(ev, ical.PropDescription),
		},
	}
	if start, err := ev.DateTimeStart(time.UTC); err == nil {
		e.Start = start
	}
	if end, err := ev.DateTimeEnd(time.UTC); err == nil {
		e.End = end
	}
	if created, err := ev.Props.DateTime(ical.PropCreated, time.UTC); err == nil {
		e.CreatedAt = created
	}
	e.IsAllDay = isAllDay(ev)
	e.Recurrence = recurrenceFromEvent(ev)
	if rid, ok := recurrenceIDOf(ev); ok {
		e.RecurrenceID = rid
		e.IsOverride = true
	}
	for _, p := range ev.Props.Values(ical.PropAttendee) {
		if a := calAddress(&p); a != (calendar.Address{}) {
			e.Attendees = append(e.Attendees, calendar.Attendee{
				Name:           a.Name,
				Email:          a.Email,
				Status:         attendeeStatus(&p),
				ScheduleStatus: p.Params.Get(paramScheduleStatus),
			})
		}
	}
	return e
}

// isAllDay reports whether an event's DTSTART is a date-only value (VALUE=DATE),
// which iCalendar uses to mark an all-day event.
func isAllDay(ev *ical.Event) bool {
	prop := ev.Props.Get(ical.PropDateTimeStart)
	return prop != nil && prop.ValueType() == ical.ValueDate
}

// propText returns a component property's text value, or "" if absent or
// unreadable.
func propText(ev *ical.Event, name string) string {
	if v, err := ev.Props.Text(name); err == nil {
		return v
	}
	return ""
}

// propInt returns a component property's integer value, or 0 if absent or
// unparseable (e.g. SEQUENCE, whose RFC 5545 default is 0).
func propInt(ev *ical.Event, name string) int {
	if p := ev.Props.Get(name); p != nil {
		if v, err := p.Int(); err == nil {
			return v
		}
	}
	return 0
}

// calAddress maps an iCalendar CAL-ADDRESS property (ORGANIZER / ATTENDEE) to a
// neutral Address. The value is a "mailto:" URI; the optional CN parameter
// carries the display name.
func calAddress(prop *ical.Prop) calendar.Address {
	if prop == nil {
		return calendar.Address{}
	}
	email := strings.TrimSpace(prop.Value)
	if i := strings.IndexByte(email, ':'); i >= 0 && strings.EqualFold(email[:i], "mailto") {
		email = email[i+1:]
	}
	return calendar.Address{
		Name:  prop.Params.Get(ical.ParamCommonName),
		Email: email,
	}
}

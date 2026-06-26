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
	"github.com/hstern/go-jscalendar"
	jsical "github.com/hstern/go-jscalendar/ical"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/davauth"
)

// Options configures the CalDAV connection.
type Options struct {
	// HTTPClient performs the underlying requests. When nil, http.DefaultClient
	// is used. Supply a custom client to control TLS, timeouts, or proxies.
	HTTPClient webdav.HTTPClient
	// BearerToken, when non-empty, authenticates every request with
	// Authorization: Bearer (the per-identity path, MB720-44) instead of HTTP
	// Basic — the Dial username and password are then ignored. The token is an
	// exchanged backend-audience access token.
	BearerToken string
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
// authenticating every request with HTTP Basic credentials — or, when
// Options.BearerToken is set, with Authorization: Bearer (the per-identity path).
// It does not perform any network I/O itself — discovery happens lazily on the
// first call.
func Dial(endpoint, username, password string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if o.BearerToken != "" {
		httpClient = davauth.BearerHTTPClient(httpClient, o.BearerToken)
	} else {
		httpClient = webdav.HTTPClientWithBasicAuth(httpClient, username, password)
	}

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
		if e, ok := eventFromObject(calID, obj.Path, obj.ETag, obj.Data); ok {
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
		if e, ok := eventFromObject(calID, obj.Path, obj.ETag, obj.Data); ok && e.UID == uid {
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
		e, ok := instanceFromObject(calID, objectPath, obj.ETag, obj.Data, recurrenceID)
		if !ok {
			return calendar.Event{}, fmt.Errorf("caldav: event %s has no such occurrence", id)
		}
		return e, nil
	}
	e, ok := eventFromObject(calID, objectPath, obj.ETag, obj.Data)
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
func instanceFromObject(calID, objectPath, etag string, cal *ical.Calendar, recurrenceID time.Time) (calendar.Event, bool) {
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
				setRecurrenceID(&e, recurrenceID)
				e.ID = instanceEventID(objectPath, recurrenceID)
				e.CalendarID = calID
				e.SeriesMasterID = eventID(objectPath)
				e.ETag = etag
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
	e.RecurrenceRules = nil // a single occurrence is not itself a series
	start, _ := master.DateTimeStart(time.UTC)
	end, _ := master.DateTimeEnd(time.UTC)
	if !start.IsZero() && !end.IsZero() {
		e.SetUTCTimes(recurrenceID, recurrenceID.Add(end.Sub(start)))
	} else {
		e.SetUTCTimes(recurrenceID, time.Time{})
	}
	setRecurrenceID(&e, recurrenceID)
	e.ID = instanceEventID(objectPath, recurrenceID)
	e.CalendarID = calID
	e.SeriesMasterID = eventID(objectPath)
	e.ETag = etag
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
func eventFromObject(calID, objectPath, etag string, cal *ical.Calendar) (calendar.Event, bool) {
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
	e.ETag = etag
	return e, true
}

// mapEvent maps an iCalendar VEVENT to a neutral Event by routing it through the
// go-jscalendar/ical bridge — the single place iCal↔JSCalendar mapping lives — and
// then reconciling the two things the bridge does not carry: each attendee's
// PARTSTAT (its participationStatus) and the override RECURRENCE-ID. It is
// best-effort: a VEVENT the bridge cannot convert yields a near-empty Event rather
// than failing. ID, CalendarID, and SeriesMasterID are left to the caller.
func mapEvent(ev *ical.Event) calendar.Event {
	jsEvent := bridgeFromVEVENT(ev)
	e := calendar.Event{Event: jsEvent}
	reconcilePartStat(&e, ev)
	if rid, ok := recurrenceIDOf(ev); ok {
		setRecurrenceID(&e, rid)
		e.IsOverride = true
	}
	return e
}

// bridgeFromVEVENT runs one VEVENT through the ical bridge and returns the
// resulting jscalendar.Event. The bridge consumes a whole VCALENDAR, so the
// VEVENT is wrapped in a minimal one; a convert failure (or a VCALENDAR yielding
// no event) degrades to an empty event so a single bad component does not fail the
// listing.
func bridgeFromVEVENT(ev *ical.Event) jscalendar.Event {
	cal := ical.NewCalendar()
	cal.Children = append(cal.Children, ev.Component)
	objs, err := jsical.FromICal(cal)
	if err != nil {
		return jscalendar.Event{}
	}
	for _, o := range objs {
		if jsEvent, ok := o.(*jscalendar.Event); ok {
			return *jsEvent
		}
	}
	return jscalendar.Event{}
}

// reconcilePartStat copies each ATTENDEE's PARTSTAT and RFC 6638 SCHEDULE-STATUS —
// which the ical bridge drops — onto the matching participant's
// participationStatus and ScheduleStatus, matching by scheduling address
// (CAL-ADDRESS / sendTo imip). Without this the read path would lose the
// attendee's response and the recorded scheduling-delivery outcome.
func reconcilePartStat(e *calendar.Event, ev *ical.Event) {
	if len(e.Participants) == 0 {
		return
	}
	type attendeeAttrs struct{ partStat, schedStatus string }
	byAddr := make(map[string]attendeeAttrs)
	for _, p := range ev.Props.Values(ical.PropAttendee) {
		a := attendeeAttrs{partStat: attendeePartStat(&p), schedStatus: attendeeScheduleStatus(&p)}
		if a.partStat == "" && a.schedStatus == "" {
			continue
		}
		byAddr[normalizeCalAddress(p.Value)] = a
	}
	if len(byAddr) == 0 {
		return
	}
	for id, p := range e.Participants {
		addr := p.SendTo["imip"]
		if addr == "" && p.Email != "" {
			addr = "mailto:" + p.Email
		}
		a, ok := byAddr[normalizeCalAddress(addr)]
		if !ok {
			continue
		}
		changed := false
		if a.partStat != "" {
			p.ParticipationStatus = a.partStat
			changed = true
		}
		if a.schedStatus != "" {
			p.ScheduleStatus = []string{a.schedStatus}
			changed = true
		}
		if changed {
			e.Participants[id] = p
		}
	}
}

// normalizeCalAddress lowercases a CAL-ADDRESS for matching, since iCalendar
// addresses are case-insensitive in the mailto scheme.
func normalizeCalAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

// setRecurrenceID stamps the override RECURRENCE-ID instant onto the envelope as a
// UTC LocalDateTime + zone, the JSCalendar shape for an expanded standalone
// override (RFC 8984 §4.3.1–4.3.2). The adapter reads and writes RECURRENCE-ID in
// UTC, so the zone is always "Etc/UTC".
func setRecurrenceID(e *calendar.Event, rid time.Time) {
	ldt := localDateTimeFromUTC(rid.UTC())
	e.RecurrenceID = &ldt
	e.RecurrenceIDTimeZone = "Etc/UTC"
}

// recurrenceIDTime resolves the envelope's RECURRENCE-ID LocalDateTime back to a
// UTC instant, the form the recurrence-expansion code compares against. It returns
// the zero time when no RECURRENCE-ID is set.
func recurrenceIDTime(e calendar.Event) time.Time {
	if e.RecurrenceID == nil {
		return time.Time{}
	}
	r := e.RecurrenceID
	loc := time.UTC
	if e.RecurrenceIDTimeZone != "" {
		if l, err := time.LoadLocation(string(e.RecurrenceIDTimeZone)); err == nil {
			loc = l
		}
	}
	return time.Date(r.Year, time.Month(r.Month), r.Day, r.Hour, r.Minute, r.Second, 0, loc).UTC()
}

// localDateTimeFromUTC builds a JSCalendar LocalDateTime from a UTC instant's
// wall-clock fields.
func localDateTimeFromUTC(t time.Time) jscalendar.LocalDateTime {
	return jscalendar.LocalDateTime{
		Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(),
	}
}

// propText returns a component property's text value, or "" if absent or
// unreadable.
func propText(ev *ical.Event, name string) string {
	if v, err := ev.Props.Text(name); err == nil {
		return v
	}
	return ""
}

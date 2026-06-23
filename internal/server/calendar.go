package server

import (
	"context"
	"fmt"
	"net/http"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-jscalendar"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// jscalParticipant aliases the JSCalendar participant the neutral calendar.Event
// now carries, so the Graph-mapping helpers in this package read without repeating
// the package-qualified name.
type jscalParticipant = jscalendar.Participant

// CalendarProvider yields a calendar.Backend for an authenticated request. The
// static implementation lives in cmd/mailboxd; per-identity providers (mapping
// the token's mailbox identity to backend credentials) come later. It mirrors
// MailProvider.
type CalendarProvider interface {
	Calendar(ctx context.Context) (calendar.Backend, error)
}

// calendarBackend resolves the request's calendar backend, or reports "not
// implemented" when no provider is configured (the skeleton posture). Mirrors
// Handler.backend for the mail port.
func (h Handler) calendarBackend(ctx context.Context) (calendar.Backend, error) {
	if h.calendar == nil {
		return nil, ht.ErrNotImplemented
	}
	return h.calendar.Calendar(ctx)
}

// MeListEvents implements GET /me/events by listing the events of the principal's
// default calendar. The Graph /me/events collection is the user's primary
// calendar; the CalDAV port addresses events by calendar, so this resolves the
// first (primary) calendar and lists its events — analogous to /me/messages
// defaulting to the inbox.
func (h Handler) MeListEvents(ctx context.Context, params api.MeListEventsParams) (api.MeListEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	calID, ok, err := defaultCalendarID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No calendars for this principal — no events, rather than querying with
		// an empty (invalid) calendar id.
		return &api.MicrosoftGraphEventCollectionResponseStatusCode{
			StatusCode: http.StatusOK,
			Response:   api.MicrosoftGraphEventCollectionResponse{Value: []api.MicrosoftGraphEvent{}},
		}, nil
	}

	events, err := b.ListEvents(ctx, calID, calendar.Range{})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	value := make([]api.MicrosoftGraphEvent, 0, len(events))
	for _, e := range events {
		value = append(value, toGraphEvent(e))
	}
	return &api.MicrosoftGraphEventCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphEventCollectionResponse{Value: value},
	}, nil
}

// MeGetEvents implements GET /me/events/{event-id}.
func (h Handler) MeGetEvents(ctx context.Context, params api.MeGetEventsParams) (api.MeGetEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	e, err := b.GetEvent(ctx, params.EventID)
	if err != nil {
		return nil, fmt.Errorf("get event: %w", err)
	}
	return &api.MicrosoftGraphEventStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphEvent(e),
	}, nil
}

// MeListCalendars implements GET /me/calendars.
func (h Handler) MeListCalendars(ctx context.Context, _ api.MeListCalendarsParams) (api.MeListCalendarsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	cals, err := b.ListCalendars(ctx)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	value := make([]api.MicrosoftGraphCalendar, 0, len(cals))
	for _, c := range cals {
		value = append(value, toGraphCalendar(c))
	}
	return &api.MicrosoftGraphCalendarCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphCalendarCollectionResponse{Value: value},
	}, nil
}

// MeGetCalendars implements GET /me/calendars/{calendar-id}. The port lists
// calendars rather than fetching one by ID, so this resolves the request from
// the principal's calendar set.
func (h Handler) MeGetCalendars(ctx context.Context, params api.MeGetCalendarsParams) (api.MeGetCalendarsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	cals, err := b.ListCalendars(ctx)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	for _, c := range cals {
		if c.ID == params.CalendarID {
			return &api.MicrosoftGraphCalendarStatusCode{
				StatusCode: http.StatusOK,
				Response:   toGraphCalendar(c),
			}, nil
		}
	}
	return nil, ht.ErrNotImplemented
}

// toGraphEvent maps the neutral calendar.Event (an embedded jscalendar.Event plus
// our routing IDs) onto the generated Graph type. Times are emitted via the
// tz-preserving StartGraph/EndGraph helpers (RFC 8984 named zones survive rather
// than collapsing to UTC); participants, recurrence, and body come from the
// JSCalendar accessors in internal/calendar.
func toGraphEvent(e calendar.Event) api.MicrosoftGraphEvent {
	ge := api.MicrosoftGraphEvent{
		ID:        api.NewOptString(e.ID),
		Subject:   api.NewOptNilString(e.Title),
		IsAllDay:  api.NewOptNilBool(e.ShowWithoutTime),
		Attendees: toGraphAttendees(e.Attendees()),
	}
	if dt, zone, ok := e.StartGraph(); ok {
		ge.Start = api.NewOptMicrosoftGraphDateTimeTimeZone(graphDateTime(dt, zone))
	}
	if dt, zone, ok := e.EndGraph(); ok {
		ge.End = api.NewOptMicrosoftGraphDateTimeTimeZone(graphDateTime(dt, zone))
	}
	if e.Created != nil {
		ge.CreatedDateTime = api.NewOptNilDateTime(e.Created.Time())
	}
	if loc := eventLocation(e); loc != "" {
		ge.Location = api.NewOptMicrosoftGraphLocation(api.MicrosoftGraphLocation{
			DisplayName: api.NewOptNilString(loc),
		})
	}
	if org, ok := e.Organizer(); ok && calendar.ParticipantEmail(org) != "" {
		ge.Organizer = api.NewOptMicrosoftGraphRecipient(toGraphRecipient(org))
	}
	if e.Description != "" {
		ge.Body = api.NewOptMicrosoftGraphItemBody(api.MicrosoftGraphItemBody{
			Content:     api.NewOptNilString(e.Description),
			ContentType: api.NewOptMicrosoftGraphBodyType(graphBodyType(neutralBodyKind(e.DescriptionContentType))),
		})
	}
	if pr, ok := graphRecurrence(e); ok {
		ge.Recurrence = api.NewOptMicrosoftGraphPatternedRecurrence(pr)
	}
	// Expanded occurrences and exceptions carry the link back to their series
	// master; surface it so a client can navigate from an instance to its series.
	if e.SeriesMasterID != "" {
		ge.SeriesMasterId = api.NewOptNilString(e.SeriesMasterID)
	}
	ge.Type = api.NewOptMicrosoftGraphEventType(graphEventType(e))
	return ge
}

// graphDateTime wraps a Graph dateTime string + IANA zone name (as produced by
// Event.StartGraph/EndGraph) in the Graph dateTimeTimeZone shape. Unlike the
// UTC-only pivot it replaces, the event's named time zone is carried through.
func graphDateTime(dateTime, timeZone string) api.MicrosoftGraphDateTimeTimeZone {
	return api.MicrosoftGraphDateTimeTimeZone{
		DateTime: api.NewOptString(dateTime),
		TimeZone: api.NewOptNilString(timeZone),
	}
}

// eventLocation returns the display name of the event's first location, or "" when
// it has none. The neutral model carries a map of locations keyed by JSCalendar Id;
// the Graph single-location surface takes the one we build under key "1".
func eventLocation(e calendar.Event) string {
	if loc, ok := e.Locations["1"]; ok {
		return loc.Name
	}
	for _, loc := range e.Locations {
		return loc.Name
	}
	return ""
}

// neutralBodyKind reduces a JSCalendar descriptionContentType media type (e.g.
// "text/html", "text/plain") to the "html"/"text" token graphBodyType expects.
func neutralBodyKind(mediaType string) string {
	if mediaType == "text/html" || mediaType == "html" {
		return "html"
	}
	return "text"
}

func toGraphRecipient(p jscalParticipant) api.MicrosoftGraphRecipient {
	return api.MicrosoftGraphRecipient{
		EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
			Name:    api.NewOptNilString(p.Name),
			Address: api.NewOptNilString(calendar.ParticipantEmail(p)),
		}),
	}
}

func toGraphAttendees(as []jscalParticipant) []api.MicrosoftGraphAttendee {
	if len(as) == 0 {
		return nil
	}
	out := make([]api.MicrosoftGraphAttendee, 0, len(as))
	for _, a := range as {
		att := api.MicrosoftGraphAttendee{
			EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
				Name:    api.NewOptNilString(a.Name),
				Address: api.NewOptNilString(calendar.ParticipantEmail(a)),
			}),
		}
		// Map the JSCalendar participationStatus onto Graph's responseStatus.response.
		if a.ParticipationStatus != "" {
			att.Status = api.NewOptMicrosoftGraphResponseStatus(api.MicrosoftGraphResponseStatus{
				Response: api.NewOptMicrosoftGraphResponseType(api.MicrosoftGraphResponseType(calendar.PartStatToResponse(a.ParticipationStatus))),
			})
		}
		out = append(out, att)
	}
	return out
}

// toGraphCalendar maps the neutral calendar.Calendar onto the generated Graph type.
func toGraphCalendar(c calendar.Calendar) api.MicrosoftGraphCalendar {
	return api.MicrosoftGraphCalendar{
		ID:   api.NewOptString(c.ID),
		Name: api.NewOptNilString(c.Name),
	}
}

// defaultCalendarID resolves the principal's primary calendar — the first
// calendar the backend reports. The CalDAV port has no "default folder"
// shortcut the way mail uses an empty folder ID for the inbox, so /me/events
// resolves it explicitly. ok is false when the principal has no calendars; the
// caller must not query with the empty id (it is not a valid calendar).
func defaultCalendarID(ctx context.Context, b calendar.Backend) (id string, ok bool, err error) {
	cals, err := b.ListCalendars(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list calendars: %w", err)
	}
	if len(cals) == 0 {
		return "", false, nil
	}
	return cals[0].ID, true, nil
}

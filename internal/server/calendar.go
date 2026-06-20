package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

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

// toGraphEvent maps the neutral calendar.Event onto the generated Graph type.
func toGraphEvent(e calendar.Event) api.MicrosoftGraphEvent {
	ge := api.MicrosoftGraphEvent{
		ID:        api.NewOptString(e.ID),
		Subject:   api.NewOptNilString(e.Subject),
		IsAllDay:  api.NewOptNilBool(e.IsAllDay),
		Attendees: toGraphAttendees(e.Attendees),
	}
	if !e.Start.IsZero() {
		ge.Start = api.NewOptMicrosoftGraphDateTimeTimeZone(graphDateTime(e.Start))
	}
	if !e.End.IsZero() {
		ge.End = api.NewOptMicrosoftGraphDateTimeTimeZone(graphDateTime(e.End))
	}
	if !e.CreatedAt.IsZero() {
		ge.CreatedDateTime = api.NewOptNilDateTime(e.CreatedAt)
	}
	if e.Location != "" {
		ge.Location = api.NewOptMicrosoftGraphLocation(api.MicrosoftGraphLocation{
			DisplayName: api.NewOptNilString(e.Location),
		})
	}
	if e.Organizer.Email != "" {
		ge.Organizer = api.NewOptMicrosoftGraphRecipient(toCalendarRecipient(e.Organizer))
	}
	if e.Body.Content != "" {
		ge.Body = api.NewOptMicrosoftGraphItemBody(api.MicrosoftGraphItemBody{
			Content:     api.NewOptNilString(e.Body.Content),
			ContentType: api.NewOptMicrosoftGraphBodyType(graphBodyType(e.Body.ContentType)),
		})
	}
	if pr, ok := graphRecurrence(e.Recurrence, e.Start); ok {
		ge.Recurrence = api.NewOptMicrosoftGraphPatternedRecurrence(pr)
	}
	ge.Type = api.NewOptMicrosoftGraphEventType(graphEventType(e))
	return ge
}

// graphDateTime maps an instant onto the Graph dateTimeTimeZone shape. CalDAV
// instants are normalized to UTC by the port, so the time zone is reported as
// UTC and the date-time is rendered in the Graph format ({date}T{time}).
func graphDateTime(t time.Time) api.MicrosoftGraphDateTimeTimeZone {
	return api.MicrosoftGraphDateTimeTimeZone{
		DateTime: api.NewOptString(t.UTC().Format("2006-01-02T15:04:05.0000000")),
		TimeZone: api.NewOptNilString("UTC"),
	}
}

func toCalendarRecipient(a calendar.Address) api.MicrosoftGraphRecipient {
	return api.MicrosoftGraphRecipient{
		EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
			Name:    api.NewOptNilString(a.Name),
			Address: api.NewOptNilString(a.Email),
		}),
	}
}

func toGraphAttendees(as []calendar.Attendee) []api.MicrosoftGraphAttendee {
	if len(as) == 0 {
		return nil
	}
	out := make([]api.MicrosoftGraphAttendee, 0, len(as))
	for _, a := range as {
		att := api.MicrosoftGraphAttendee{
			EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
				Name:    api.NewOptNilString(a.Name),
				Address: api.NewOptNilString(a.Email),
			}),
		}
		// The neutral status uses the same tokens as Graph's responseStatus.response.
		if a.Status != "" {
			att.Status = api.NewOptMicrosoftGraphResponseStatus(api.MicrosoftGraphResponseStatus{
				Response: api.NewOptMicrosoftGraphResponseType(api.MicrosoftGraphResponseType(a.Status)),
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

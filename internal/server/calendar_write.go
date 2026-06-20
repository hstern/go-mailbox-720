package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// MeCreateEvents implements POST /me/events. It maps the inbound Graph event
// body onto the neutral calendar.Event, creates it in the principal's primary
// calendar, and returns the stored event (201 Created). The backend is obtained
// via calendarBackend (nil-provider -> 501) and type-asserted to calendar.Writer;
// a read-only backend yields 501.
//
// Deferred: MeUpdateEvents (PATCH) needs a read-modify-write merge of the partial
// body onto the stored event and is left for a follow-up.
func (h Handler) MeCreateEvents(ctx context.Context, req *api.MicrosoftGraphEvent) (api.MeCreateEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(calendar.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	event, err := graphToEvent(req)
	if err != nil {
		return badRequest(err.Error()), nil
	}

	calID, ok, err := defaultCalendarID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No calendar to create the event in; nothing to write against.
		return nil, ht.ErrNotImplemented
	}

	created, err := w.CreateEvent(ctx, calID, event)
	if err != nil {
		return nil, fmt.Errorf("create event: %w", err)
	}
	return &api.MicrosoftGraphEventStatusCode{
		StatusCode: http.StatusCreated,
		Response:   toGraphEvent(created),
	}, nil
}

// MeDeleteEvents implements DELETE /me/events/{event-id}. It type-asserts the
// backend to calendar.Writer (read-only backend -> 501) and deletes the event,
// returning 204 No Content.
func (h Handler) MeDeleteEvents(ctx context.Context, params api.MeDeleteEventsParams) (api.MeDeleteEventsRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(calendar.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	if err := w.DeleteEvent(ctx, params.EventID); err != nil {
		return nil, fmt.Errorf("delete event: %w", err)
	}
	return &api.MeDeleteEventsNoContent{}, nil
}

// graphToEvent maps the inbound Graph event onto the neutral calendar.Event — the
// inverse of toGraphEvent. Read-only and server-assigned fields (ID, ICalUId) are
// ignored: the backend stamps the created event with its own opaque ID/UID.
func graphToEvent(ge *api.MicrosoftGraphEvent) (calendar.Event, error) {
	e := calendar.Event{
		Subject:   ge.Subject.Or(""),
		IsAllDay:  ge.IsAllDay.Or(false),
		Attendees: graphToAddresses(ge.Attendees),
	}
	if v, ok := ge.Start.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("start: %w", err)
		}
		e.Start = t
	}
	if v, ok := ge.End.Get(); ok {
		t, err := graphToTime(v)
		if err != nil {
			return calendar.Event{}, fmt.Errorf("end: %w", err)
		}
		e.End = t
	}
	if v, ok := ge.Location.Get(); ok {
		e.Location = v.DisplayName.Or("")
	}
	if v, ok := ge.Organizer.Get(); ok {
		e.Organizer = graphRecipientToAddress(v)
	}
	if v, ok := ge.Body.Get(); ok {
		e.Body = calendar.Body{
			Content:     v.Content.Or(""),
			ContentType: neutralBodyType(v.ContentType),
		}
	}
	return e, nil
}

// graphToTime parses a Graph dateTimeTimeZone back into an instant — the inverse
// of graphDateTime. An RFC3339 dateTime carries its own offset and is honored as
// given. Otherwise the dateTime is a naive wall-clock that we can only place on
// the timeline if its timeZone is UTC (or absent): Graph sends Windows zone names
// like "Pacific Standard Time", and rather than silently storing the wrong
// instant we reject a non-UTC zone until a Windows->IANA mapping lands. An absent
// or unparseable dateTime yields the zero time, which the backend treats as unset.
func graphToTime(dt api.MicrosoftGraphDateTimeTimeZone) (time.Time, error) {
	s, ok := dt.DateTime.Get()
	if !ok || s == "" {
		return time.Time{}, nil
	}
	// An RFC3339 instant fixes its own offset; honor it regardless of timeZone.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	// Naive wall-clock: only UTC (or an unspecified zone) can be placed exactly.
	if tz, ok := dt.TimeZone.Get(); ok && tz != "" && !strings.EqualFold(tz, "UTC") {
		return time.Time{}, fmt.Errorf("unsupported time zone %q: only UTC is supported", tz)
	}
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, nil
}

// graphRecipientToAddress maps a Graph recipient onto a calendar.Address.
func graphRecipientToAddress(r api.MicrosoftGraphRecipient) calendar.Address {
	ea, ok := r.EmailAddress.Get()
	if !ok {
		return calendar.Address{}
	}
	return calendar.Address{
		Name:  ea.Name.Or(""),
		Email: ea.Address.Or(""),
	}
}

// graphToAddresses maps Graph attendees onto neutral calendar.Address values.
func graphToAddresses(as []api.MicrosoftGraphAttendee) []calendar.Address {
	if len(as) == 0 {
		return nil
	}
	out := make([]calendar.Address, 0, len(as))
	for _, a := range as {
		ea, ok := a.EmailAddress.Get()
		if !ok {
			continue
		}
		out = append(out, calendar.Address{
			Name:  ea.Name.Or(""),
			Email: ea.Address.Or(""),
		})
	}
	return out
}

// neutralBodyType maps a Graph bodyType back onto the neutral "text"/"html"
// string — the inverse of graphBodyType.
func neutralBodyType(bt api.OptMicrosoftGraphBodyType) string {
	if v, ok := bt.Get(); ok && v == api.MicrosoftGraphBodyTypeHTML {
		return "html"
	}
	return "text"
}

package server

import (
	"context"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// writableCalendarBackend implements BOTH calendar.Backend and calendar.Writer,
// recording write calls so tests can assert the handler reached the Writer with
// the mapped arguments.
type writableCalendarBackend struct {
	fakeCalendarBackend

	createdCalID string
	createdEvent calendar.Event
	deletedID    string
}

func (f *writableCalendarBackend) CreateEvent(_ context.Context, calendarID string, e calendar.Event) (calendar.Event, error) {
	f.createdCalID = calendarID
	f.createdEvent = e
	e.ID = "evt-new" // the backend stamps an opaque ID.
	return e, nil
}

func (f *writableCalendarBackend) UpdateEvent(_ context.Context, e calendar.Event) (calendar.Event, error) {
	return e, nil
}

func (f *writableCalendarBackend) DeleteEvent(_ context.Context, id string) error {
	f.deletedID = id
	return nil
}

func newWritableCalendarBackend() *writableCalendarBackend {
	return &writableCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{
			calendars: []calendar.Calendar{{ID: "cal-primary", Name: "Calendar"}},
		},
	}
}

func TestMeCreateEventsMapsBodyAndCallsWriter(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Subject:  api.NewOptNilString("Planning"),
		Location: api.NewOptMicrosoftGraphLocation(api.MicrosoftGraphLocation{DisplayName: api.NewOptNilString("Room 7")}),
		Start:    api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{DateTime: api.NewOptString("2026-06-20T09:00:00.0000000")}),
		Attendees: []api.MicrosoftGraphAttendee{{
			EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
				Name:    api.NewOptNilString("Bob"),
				Address: api.NewOptNilString("bob@example.com"),
			}),
		}},
	}

	res, err := h.MeCreateEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphEventStatusCode", res)
	}
	if ok.StatusCode != 201 {
		t.Errorf("status = %d, want 201", ok.StatusCode)
	}
	if backend.createdCalID != "cal-primary" {
		t.Errorf("create calendar id = %q, want cal-primary", backend.createdCalID)
	}
	if got := backend.createdEvent.Subject; got != "Planning" {
		t.Errorf("mapped subject = %q, want Planning", got)
	}
	if got := backend.createdEvent.Location; got != "Room 7" {
		t.Errorf("mapped location = %q, want Room 7", got)
	}
	if got := len(backend.createdEvent.Attendees); got != 1 {
		t.Fatalf("attendee count = %d, want 1", got)
	}
	if got := backend.createdEvent.Attendees[0].Email; got != "bob@example.com" {
		t.Errorf("attendee email = %q, want bob@example.com", got)
	}
	if backend.createdEvent.Start.IsZero() {
		t.Error("mapped start is zero, want parsed instant")
	}
	if got := ok.Response.ID.Or(""); got != "evt-new" {
		t.Errorf("returned event id = %q, want evt-new (backend-stamped)", got)
	}
	if !backend.closed {
		t.Error("backend not closed")
	}
}

func TestMeDeleteEventsCallsWriter(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	res, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeDeleteEvents: %v", err)
	}
	if _, ok := res.(*api.MeDeleteEventsNoContent); !ok {
		t.Fatalf("response type = %T, want *MeDeleteEventsNoContent (204)", res)
	}
	if backend.deletedID != "evt-1" {
		t.Errorf("deleted id = %q, want evt-1", backend.deletedID)
	}
}

// A read-only backend (Backend but not Writer) must yield the not-implemented
// sentinel (rendered as a Graph 501) for both create and delete.
func TestMeCreateEventsReadOnlyBackendNotImplemented(t *testing.T) {
	backend := newCalendarFixture() // *fakeCalendarBackend: Backend only, no Writer
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	if _, err := h.MeCreateEvents(context.Background(), &api.MicrosoftGraphEvent{}); err == nil {
		t.Error("MeCreateEvents on read-only backend: expected error, got nil")
	}
	if _, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"}); err == nil {
		t.Error("MeDeleteEvents on read-only backend: expected error, got nil")
	}
}

func TestNilCalendarProviderWriteNotImplemented(t *testing.T) {
	h := Handler{}
	if _, err := h.MeCreateEvents(context.Background(), &api.MicrosoftGraphEvent{}); err == nil {
		t.Error("MeCreateEvents with nil provider: expected error, got nil")
	}
	if _, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "x"}); err == nil {
		t.Error("MeDeleteEvents with nil provider: expected error, got nil")
	}
}

// writableCalendarProvider hands out a writableCalendarBackend (Backend+Writer).
type writableCalendarProvider struct {
	backend *writableCalendarBackend
}

func (p writableCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

// TestMeCreateEventsRejectsNonUTCTimeZone: a naive wall-clock dateTime with a
// non-UTC (Windows) zone name must be rejected with a 400 rather than silently
// stored at the wrong instant.
func TestMeCreateEventsRejectsNonUTCTimeZone(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Subject: api.NewOptNilString("Planning"),
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{
			DateTime: api.NewOptString("2026-06-20T09:00:00.0000000"),
			TimeZone: api.NewOptNilString("Pacific Standard Time"),
		}),
	}

	res, err := h.MeCreateEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	errRes, ok := res.(*api.ErrorStatusCode)
	if !ok {
		t.Fatalf("response type = %T, want *ErrorStatusCode", res)
	}
	if errRes.StatusCode != 400 {
		t.Errorf("status = %d, want 400", errRes.StatusCode)
	}
	if backend.createdCalID != "" {
		t.Error("CreateEvent was called despite an unsupported time zone")
	}
}

// TestMeCreateEventsHonorsRFC3339Offset: a dateTime carrying an explicit offset
// is placed at the correct UTC instant regardless of any timeZone field.
func TestMeCreateEventsHonorsRFC3339Offset(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Subject: api.NewOptNilString("Planning"),
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{
			DateTime: api.NewOptString("2026-06-20T09:00:00-08:00"),
		}),
	}

	if _, err := h.MeCreateEvents(context.Background(), req); err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	want := time.Date(2026, 6, 20, 17, 0, 0, 0, time.UTC) // 09:00 -08:00 == 17:00Z
	if got := backend.createdEvent.Start; !got.Equal(want) {
		t.Errorf("mapped start = %v, want %v", got, want)
	}
}

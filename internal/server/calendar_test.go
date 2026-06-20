package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// fakeCalendarBackend is an in-memory calendar.Backend returning canned data.
type fakeCalendarBackend struct {
	calendars []calendar.Calendar
	events    map[string][]calendar.Event // keyed by calendar ID
	closed    bool
}

func (f *fakeCalendarBackend) ListCalendars(_ context.Context) ([]calendar.Calendar, error) {
	return f.calendars, nil
}

func (f *fakeCalendarBackend) ListEvents(_ context.Context, calendarID string, _ calendar.Range) ([]calendar.Event, error) {
	return f.events[calendarID], nil
}

func (f *fakeCalendarBackend) GetEvent(_ context.Context, id string) (calendar.Event, error) {
	for _, evs := range f.events {
		for _, e := range evs {
			if e.ID == id {
				return e, nil
			}
		}
	}
	// Match the real adapter, which errors on a missing id rather than returning
	// a zero event.
	return calendar.Event{}, fmt.Errorf("event %q not found", id)
}

func (f *fakeCalendarBackend) Close() error {
	f.closed = true
	return nil
}

// fakeCalendarProvider hands out one fakeCalendarBackend.
type fakeCalendarProvider struct {
	backend *fakeCalendarBackend
}

func (p fakeCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

func newCalendarFixture() *fakeCalendarBackend {
	start := time.Date(2026, 6, 19, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	return &fakeCalendarBackend{
		calendars: []calendar.Calendar{
			{ID: "cal-primary", Name: "Calendar"},
			{ID: "cal-work", Name: "Work"},
		},
		events: map[string][]calendar.Event{
			"cal-primary": {
				{
					ID:        "evt-1",
					Subject:   "Standup",
					Start:     start,
					End:       end,
					Location:  "Room 1",
					Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
					Attendees: []calendar.Attendee{{Name: "Bob", Email: "bob@example.com"}},
				},
			},
		},
	}
}

func TestMeListEventsMapsGraphEvents(t *testing.T) {
	backend := newCalendarFixture()
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	res, err := h.MeListEvents(context.Background(), api.MeListEventsParams{})
	if err != nil {
		t.Fatalf("MeListEvents: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphEventCollectionResponseStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if got := len(ok.Response.Value); got != 1 {
		t.Fatalf("event count = %d, want 1", got)
	}
	ev := ok.Response.Value[0]
	if got := ev.Subject.Or(""); got != "Standup" {
		t.Errorf("subject = %q, want Standup", got)
	}
	if got := ev.Start.Value.DateTime.Or(""); got != "2026-06-19T14:00:00.0000000" {
		t.Errorf("start dateTime = %q, want 2026-06-19T14:00:00.0000000", got)
	}
	if got := ev.End.Value.DateTime.Or(""); got != "2026-06-19T15:00:00.0000000" {
		t.Errorf("end dateTime = %q, want 2026-06-19T15:00:00.0000000", got)
	}
	if got := ev.Location.Value.DisplayName.Or(""); got != "Room 1" {
		t.Errorf("location = %q, want Room 1", got)
	}
	if !backend.closed {
		t.Errorf("backend not closed")
	}
}

// A principal with no calendars yields an empty event list (200), not a query
// against an empty/invalid calendar id.
func TestMeListEventsNoCalendars(t *testing.T) {
	backend := &fakeCalendarBackend{} // no calendars, no events
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	res, err := h.MeListEvents(context.Background(), api.MeListEventsParams{})
	if err != nil {
		t.Fatalf("MeListEvents: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want collection", res)
	}
	if ok.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if got := len(ok.Response.Value); got != 0 {
		t.Errorf("event count = %d, want 0", got)
	}
}

func TestMeListCalendarsMapsGraphCalendars(t *testing.T) {
	backend := newCalendarFixture()
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	res, err := h.MeListCalendars(context.Background(), api.MeListCalendarsParams{})
	if err != nil {
		t.Fatalf("MeListCalendars: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphCalendarCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphCalendarCollectionResponseStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if got := len(ok.Response.Value); got != 2 {
		t.Fatalf("calendar count = %d, want 2", got)
	}
	if got := ok.Response.Value[0].Name.Or(""); got != "Calendar" {
		t.Errorf("calendar[0] name = %q, want Calendar", got)
	}
	if got := ok.Response.Value[0].ID.Or(""); got != "cal-primary" {
		t.Errorf("calendar[0] id = %q, want cal-primary", got)
	}
}

func TestMeGetEventsMapsGraphEvent(t *testing.T) {
	backend := newCalendarFixture()
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	res, err := h.MeGetEvents(context.Background(), api.MeGetEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeGetEvents: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphEventStatusCode", res)
	}
	if got := ok.Response.Subject.Or(""); got != "Standup" {
		t.Errorf("subject = %q, want Standup", got)
	}
}

// A nil calendar provider must yield the ogen "not implemented" sentinel, which
// the error handler renders as a Graph 501 (see server_test.go for the HTTP-level
// assertion on the mail side).
func TestNilCalendarProviderNotImplemented(t *testing.T) {
	h := Handler{}

	if _, err := h.MeListEvents(context.Background(), api.MeListEventsParams{}); err == nil {
		t.Fatal("MeListEvents: expected error, got nil")
	}
	if _, err := h.MeListCalendars(context.Background(), api.MeListCalendarsParams{}); err == nil {
		t.Fatal("MeListCalendars: expected error, got nil")
	}
}

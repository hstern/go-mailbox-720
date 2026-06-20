package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
)

// writableCalendarBackend implements BOTH calendar.Backend and calendar.Writer,
// recording write calls so tests can assert the handler reached the Writer with
// the mapped arguments.
type writableCalendarBackend struct {
	fakeCalendarBackend

	createdCalID string
	createdEvent calendar.Event
	updatedEvent calendar.Event
	deletedID    string
}

func (f *writableCalendarBackend) CreateEvent(_ context.Context, calendarID string, e calendar.Event) (calendar.Event, error) {
	f.createdCalID = calendarID
	f.createdEvent = e
	e.ID = "evt-new"  // the backend stamps an opaque ID...
	e.UID = "uid-new" // ...and an iCalendar UID, as the real CalDAV adapter does.
	return e, nil
}

func (f *writableCalendarBackend) UpdateEvent(_ context.Context, e calendar.Event) (calendar.Event, error) {
	f.updatedEvent = e
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

// seededEvent is the current event the writable backend's GetEvent returns, used
// by the PATCH (read-modify-write) tests. Its fields stand in for an existing
// stored record so a partial patch can be checked for leaving them intact.
var seededEvent = calendar.Event{
	ID:        "evt-1",
	UID:       "uid-1",
	Subject:   "Old Subject",
	Location:  "Old Room",
	Start:     time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
	End:       time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC),
	Attendees: []calendar.Attendee{{Name: "Bob", Email: "bob@example.com"}},
}

// newWritableCalendarBackendSeeded returns a writable backend whose GetEvent
// resolves seededEvent by its ID — the current record a PATCH reads, merges, and
// writes back.
func newWritableCalendarBackendSeeded() *writableCalendarBackend {
	return &writableCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{
			calendars: []calendar.Calendar{{ID: "cal-primary", Name: "Calendar"}},
			events:    map[string][]calendar.Event{"cal-primary": {seededEvent}},
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

// TestMeCreateEventsRejectsUnknownTimeZone: an unresolvable zone name is a 400
// rather than a silent mis-store.
func TestMeCreateEventsRejectsUnknownTimeZone(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Subject: api.NewOptNilString("Planning"),
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{
			DateTime: api.NewOptString("2026-06-20T09:00:00.0000000"),
			TimeZone: api.NewOptNilString("Totally Bogus Zone"),
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
		t.Error("CreateEvent was called despite an unknown time zone")
	}
}

// TestMeCreateEventsResolvesWindowsZone: a Windows zone name is resolved and the
// naive wall-clock is interpreted in that zone. 2026-06-20 is daylight time in
// the US Pacific zone (PDT, UTC-7), so 09:00 -> 16:00 UTC.
func TestMeCreateEventsResolvesWindowsZone(t *testing.T) {
	backend := newWritableCalendarBackend()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Subject: api.NewOptNilString("Planning"),
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{
			DateTime: api.NewOptString("2026-06-20T09:00:00.0000000"),
			TimeZone: api.NewOptNilString("Pacific Standard Time"),
		}),
	}
	if _, err := h.MeCreateEvents(context.Background(), req); err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	if backend.createdCalID == "" {
		t.Fatal("CreateEvent was not called (zone should resolve)")
	}
	want := time.Date(2026, 6, 20, 16, 0, 0, 0, time.UTC)
	if got := backend.createdEvent.Start; !got.Equal(want) {
		t.Errorf("mapped start = %v, want %v (09:00 Pacific = 16:00 UTC in June/PDT)", got, want)
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

// TestMeUpdateEventsPartialMergeLeavesAbsentFields: a PATCH that sets only
// Subject overlays that one field, leaving Start/End/Location/Attendees and the
// event's identity (ID/UID) intact — read-modify-write, not replace.
func TestMeUpdateEventsPartialMergeLeavesAbsentFields(t *testing.T) {
	backend := newWritableCalendarBackendSeeded()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("New Subject")}

	res, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphEventStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}

	got := backend.updatedEvent
	if got.Subject != "New Subject" {
		t.Errorf("merged subject = %q, want New Subject", got.Subject)
	}
	if got.Location != "Old Room" {
		t.Errorf("merged location = %q, want Old Room (unchanged)", got.Location)
	}
	if !got.Start.Equal(seededEvent.Start) {
		t.Errorf("merged start = %v, want %v (unchanged)", got.Start, seededEvent.Start)
	}
	if !got.End.Equal(seededEvent.End) {
		t.Errorf("merged end = %v, want %v (unchanged)", got.End, seededEvent.End)
	}
	if got.ID != "evt-1" || got.UID != "uid-1" {
		t.Errorf("merged identity = (%q,%q), want (evt-1,uid-1) preserved", got.ID, got.UID)
	}
	if n := len(got.Attendees); n != 1 || got.Attendees[0].Email != "bob@example.com" {
		t.Errorf("merged attendees = %+v, want the original one (unchanged)", got.Attendees)
	}
	if !backend.closed {
		t.Error("backend not closed")
	}
}

// TestMeUpdateEventsRejectsNonUTCTimeZone: a PATCH whose Start carries an
// unresolvable zone name must be rejected with a 400, and UpdateEvent must not run.
func TestMeUpdateEventsRejectsNonUTCTimeZone(t *testing.T) {
	backend := newWritableCalendarBackendSeeded()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{
			DateTime: api.NewOptString("2026-06-20T09:00:00.0000000"),
			TimeZone: api.NewOptNilString("Totally Bogus Zone"),
		}),
	}

	res, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	errRes, ok := res.(*api.ErrorStatusCode)
	if !ok {
		t.Fatalf("response type = %T, want *ErrorStatusCode", res)
	}
	if errRes.StatusCode != 400 {
		t.Errorf("status = %d, want 400", errRes.StatusCode)
	}
	if backend.updatedEvent.ID != "" {
		t.Error("UpdateEvent was called despite an unsupported time zone")
	}
}

// TestMeUpdateEventsReadOnlyBackendNotImplemented: a read-only backend (Backend
// but not Writer) yields the not-implemented sentinel (Graph 501).
func TestMeUpdateEventsReadOnlyBackendNotImplemented(t *testing.T) {
	backend := newCalendarFixture() // *fakeCalendarBackend: Backend only, no Writer
	h := Handler{calendar: fakeCalendarProvider{backend: backend}}

	if _, err := h.MeUpdateEvents(context.Background(), &api.MicrosoftGraphEvent{}, api.MeUpdateEventsParams{EventID: "evt-1"}); err == nil {
		t.Error("MeUpdateEvents on read-only backend: expected error, got nil")
	}
}

// TestMeUpdateEventsNilProviderNotImplemented: a nil provider yields 501.
func TestMeUpdateEventsNilProviderNotImplemented(t *testing.T) {
	h := Handler{}
	if _, err := h.MeUpdateEvents(context.Background(), &api.MicrosoftGraphEvent{}, api.MeUpdateEventsParams{EventID: "x"}); err == nil {
		t.Error("MeUpdateEvents with nil provider: expected error, got nil")
	}
}

// TestMeUpdateEventsIgnoresBodyID: the {event-id} path param is authoritative;
// a conflicting ID in the PATCH body must not redirect the write.
func TestMeUpdateEventsIgnoresBodyID(t *testing.T) {
	backend := newWritableCalendarBackendSeeded()
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{
		ID:      api.NewOptString("evt-999-attacker"),
		Subject: api.NewOptNilString("Renamed"),
	}
	if _, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: seededEvent.ID}); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if backend.updatedEvent.ID != seededEvent.ID {
		t.Errorf("updated event ID = %q, want %q (body ID must be ignored)", backend.updatedEvent.ID, seededEvent.ID)
	}
}

// createEventReqWithAttendee builds a minimal POST /me/events body inviting one
// attendee — the organizer-side scheduling tests below.
func createEventReqWithAttendee(email string) *api.MicrosoftGraphEvent {
	return &api.MicrosoftGraphEvent{
		Subject: api.NewOptNilString("Planning"),
		Start:   api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{DateTime: api.NewOptString("2026-06-20T09:00:00.0000000")}),
		Attendees: []api.MicrosoftGraphAttendee{{
			EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{Address: api.NewOptNilString(email)}),
		}},
	}
}

// On the dumb-backend tier, creating an event with attendees emails them a
// METHOD:REQUEST and persists the delivery outcome (SCHEDULE-STATUS) plus the
// creator as organizer — while the create still returns 201.
func TestMeCreateEventsEmailsInvitationOnDumbBackend(t *testing.T) {
	backend := newWritableCalendarBackend()
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeCreateEvents(context.Background(), createEventReqWithAttendee("bob@example.com"))
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 201 {
		t.Fatalf("response = %T, want 201 MicrosoftGraphEventStatusCode", res)
	}
	if sender.from != "me@example.com" {
		t.Errorf("REQUEST from = %q, want me@example.com", sender.from)
	}
	if len(sender.to) != 1 || sender.to[0] != "bob@example.com" {
		t.Errorf("REQUEST to = %v, want [bob@example.com]", sender.to)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse REQUEST: %v", err)
	}
	if inv.Method != scheduling.MethodRequest {
		t.Errorf("sent method = %q, want REQUEST", inv.Method)
	}
	if backend.updatedEvent.Organizer.Email != "me@example.com" {
		t.Errorf("persisted organizer = %q, want me@example.com", backend.updatedEvent.Organizer.Email)
	}
	if n := len(backend.updatedEvent.Attendees); n != 1 {
		t.Fatalf("persisted attendees = %d, want 1", n)
	}
	if got := backend.updatedEvent.Attendees[0].ScheduleStatus; got != "1.1" {
		t.Errorf("SCHEDULE-STATUS = %q, want 1.1 (sent)", got)
	}
}

// When the backend schedules natively (RFC 6638) the server must NOT also email a
// REQUEST, nor write a SCHEDULE-STATUS — the calendar server does both itself.
func TestMeCreateEventsNativeSchedulerDoesNotEmail(t *testing.T) {
	backend := &nativeCalendarBackend{writableCalendarBackend: *newWritableCalendarBackend()}
	sender := &fakeSender{}
	h := Handler{
		calendar:   nativeCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeCreateEvents(context.Background(), createEventReqWithAttendee("bob@example.com"))
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 201 {
		t.Fatalf("response = %T, want 201", res)
	}
	if sender.from != "" {
		t.Error("emailed a REQUEST even though the server schedules natively")
	}
	if backend.updatedEvent.ID != "" {
		t.Error("native path must not UpdateEvent to record SCHEDULE-STATUS")
	}
}

// A REQUEST that fails to send does not fail the create (still 201); the failure
// is recorded as SCHEDULE-STATUS=5.1 rather than swallowed.
func TestMeCreateEventsSendFailureRecordsNoDelivery(t *testing.T) {
	backend := newWritableCalendarBackend()
	sender := &fakeSender{sendErr: errors.New("smtp unavailable")}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeCreateEvents(context.Background(), createEventReqWithAttendee("bob@example.com"))
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 201 {
		t.Fatalf("response = %T, want 201 despite the send failure", res)
	}
	if n := len(backend.updatedEvent.Attendees); n != 1 {
		t.Fatalf("persisted attendees = %d, want 1", n)
	}
	if got := backend.updatedEvent.Attendees[0].ScheduleStatus; got != "5.1" {
		t.Errorf("SCHEDULE-STATUS = %q, want 5.1 (undeliverable)", got)
	}
}

// An event whose only attendee is the organizer invites no one (you do not invite
// yourself) and records no SCHEDULE-STATUS.
func TestMeCreateEventsSelfOnlyDoesNotEmail(t *testing.T) {
	backend := newWritableCalendarBackend()
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeCreateEvents(context.Background(), createEventReqWithAttendee("me@example.com"))
	if err != nil {
		t.Fatalf("MeCreateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 201 {
		t.Fatalf("response = %T, want 201", res)
	}
	if sender.from != "" {
		t.Error("emailed a REQUEST to the organizer's own address")
	}
	if backend.updatedEvent.ID != "" {
		t.Error("no UpdateEvent expected when there are no real recipients")
	}
}

// Deleting an event with attendees on the dumb-backend tier emails them a
// METHOD:CANCEL, while the delete itself still returns 204.
func TestMeDeleteEventsCancelsOnDumbBackend(t *testing.T) {
	backend := newWritableCalendarBackendSeeded() // GetEvent("evt-1") -> seededEvent (attendee Bob)
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeDeleteEvents: %v", err)
	}
	if _, ok := res.(*api.MeDeleteEventsNoContent); !ok {
		t.Fatalf("response = %T, want *MeDeleteEventsNoContent (204)", res)
	}
	if backend.deletedID != "evt-1" {
		t.Errorf("deleted id = %q, want evt-1", backend.deletedID)
	}
	if sender.from != "me@example.com" {
		t.Errorf("CANCEL from = %q, want me@example.com", sender.from)
	}
	if len(sender.to) != 1 || sender.to[0] != "bob@example.com" {
		t.Errorf("CANCEL to = %v, want [bob@example.com]", sender.to)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse CANCEL: %v", err)
	}
	if inv.Method != scheduling.MethodCancel {
		t.Errorf("sent method = %q, want CANCEL", inv.Method)
	}
}

// When the backend schedules natively, deleting must NOT also email a CANCEL — the
// calendar server withdraws the meeting itself.
func TestMeDeleteEventsNativeSchedulerDoesNotCancel(t *testing.T) {
	backend := &nativeCalendarBackend{writableCalendarBackend: *newWritableCalendarBackendSeeded()}
	sender := &fakeSender{}
	h := Handler{
		calendar:   nativeCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeDeleteEvents: %v", err)
	}
	if _, ok := res.(*api.MeDeleteEventsNoContent); !ok {
		t.Fatalf("response = %T, want 204", res)
	}
	if backend.deletedID != "evt-1" {
		t.Errorf("deleted id = %q, want evt-1", backend.deletedID)
	}
	if sender.from != "" {
		t.Error("emailed a CANCEL even though the server schedules natively")
	}
}

// A delete whose GetEvent fails still deletes (204) and sends no CANCEL — the
// notification is best-effort, the withdrawal is not.
func TestMeDeleteEventsGetEventFailureStillDeletes(t *testing.T) {
	backend := newWritableCalendarBackend() // no seeded events: GetEvent("evt-1") errors
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeDeleteEvents: %v", err)
	}
	if _, ok := res.(*api.MeDeleteEventsNoContent); !ok {
		t.Fatalf("response = %T, want 204", res)
	}
	if backend.deletedID != "evt-1" {
		t.Errorf("deleted id = %q, want evt-1 (delete must proceed despite the read failure)", backend.deletedID)
	}
	if sender.from != "" {
		t.Error("sent a CANCEL even though the event could not be read")
	}
}

// A CANCEL that fails to send does not fail the delete (still 204): the withdrawal
// has already happened, so the send error is logged, not propagated.
func TestMeDeleteEventsCancelSendFailureStillReturns204(t *testing.T) {
	backend := newWritableCalendarBackendSeeded()
	sender := &fakeSender{sendErr: errors.New("smtp unavailable")}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeDeleteEvents(context.Background(), api.MeDeleteEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeDeleteEvents: %v", err)
	}
	if _, ok := res.(*api.MeDeleteEventsNoContent); !ok {
		t.Fatalf("response = %T, want 204 despite the CANCEL send failure", res)
	}
	if backend.deletedID != "evt-1" {
		t.Errorf("deleted id = %q, want evt-1", backend.deletedID)
	}
}

// A PATCH that significantly changes a meeting (here, its start) re-sends a
// METHOD:REQUEST with a bumped SEQUENCE and records the per-attendee
// SCHEDULE-STATUS, on the dumb-backend tier.
func TestMeUpdateEventsSignificantChangeReinvites(t *testing.T) {
	backend := newWritableCalendarBackendSeeded() // seededEvent: Start 09:00, attendee Bob, Sequence 0
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	req := &api.MicrosoftGraphEvent{
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{DateTime: api.NewOptString("2026-06-21T11:00:00.0000000")}),
	}
	res, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 200 {
		t.Fatalf("response = %T, want 200", res)
	}
	if sender.from != "me@example.com" {
		t.Errorf("REQUEST from = %q, want me@example.com", sender.from)
	}
	if len(sender.to) != 1 || sender.to[0] != "bob@example.com" {
		t.Errorf("REQUEST to = %v, want [bob@example.com]", sender.to)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse REQUEST: %v", err)
	}
	if inv.Method != scheduling.MethodRequest {
		t.Errorf("sent method = %q, want REQUEST", inv.Method)
	}
	if inv.Sequence != 1 {
		t.Errorf("sent SEQUENCE = %d, want 1 (bumped from 0)", inv.Sequence)
	}
	if backend.updatedEvent.Sequence != 1 {
		t.Errorf("persisted SEQUENCE = %d, want 1", backend.updatedEvent.Sequence)
	}
	for _, a := range backend.updatedEvent.Attendees {
		if a.Email == "bob@example.com" && a.ScheduleStatus != "1.1" {
			t.Errorf("Bob SCHEDULE-STATUS = %q, want 1.1", a.ScheduleStatus)
		}
	}
}

// A PATCH that changes only an insignificant field (the subject) re-sends nothing
// and does not bump SEQUENCE.
func TestMeUpdateEventsInsignificantChangeDoesNotReinvite(t *testing.T) {
	backend := newWritableCalendarBackendSeeded()
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("Renamed")}
	res, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 200 {
		t.Fatalf("response = %T, want 200", res)
	}
	if sender.from != "" {
		t.Error("re-sent a REQUEST for a subject-only change")
	}
	if backend.updatedEvent.Sequence != 0 {
		t.Errorf("SEQUENCE = %d, want 0 (unchanged)", backend.updatedEvent.Sequence)
	}
}

// A native-scheduling backend re-issues invitations itself, so a significant PATCH
// must not also email a REQUEST (though SEQUENCE still advances on the write).
func TestMeUpdateEventsNativeSchedulerDoesNotReinvite(t *testing.T) {
	backend := &nativeCalendarBackend{writableCalendarBackend: *newWritableCalendarBackendSeeded()}
	sender := &fakeSender{}
	h := Handler{
		calendar:   nativeCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	req := &api.MicrosoftGraphEvent{
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{DateTime: api.NewOptString("2026-06-21T11:00:00.0000000")}),
	}
	res, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if ok, isOK := res.(*api.MicrosoftGraphEventStatusCode); !isOK || ok.StatusCode != 200 {
		t.Fatalf("response = %T, want 200", res)
	}
	if sender.from != "" {
		t.Error("emailed a REQUEST even though the server schedules natively")
	}
}

// backendWithEvent seeds a writable backend whose GetEvent resolves one event (for
// the PATCH-scheduling tests that need a specific attendee roster).
func backendWithEvent(ev calendar.Event) *writableCalendarBackend {
	return &writableCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{
			calendars: []calendar.Calendar{{ID: "cal-primary", Name: "Calendar"}},
			events:    map[string][]calendar.Event{"cal-primary": {ev}},
		},
	}
}

func attendeeReq(emails ...string) []api.MicrosoftGraphAttendee {
	out := make([]api.MicrosoftGraphAttendee, 0, len(emails))
	for _, e := range emails {
		out = append(out, api.MicrosoftGraphAttendee{
			EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{Address: api.NewOptNilString(e)}),
		})
	}
	return out
}

// A significant (start) change resets the recipients' PARTSTAT to notResponded —
// a significant change requires them to respond afresh.
func TestMeUpdateEventsSignificantChangeResetsPartStat(t *testing.T) {
	ev := seededEvent
	ev.Attendees = []calendar.Attendee{{Email: "bob@example.com", Status: "accepted"}}
	backend := backendWithEvent(ev)
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: &fakeSender{}, addr: "me@example.com"},
	}

	req := &api.MicrosoftGraphEvent{
		Start: api.NewOptMicrosoftGraphDateTimeTimeZone(api.MicrosoftGraphDateTimeTimeZone{DateTime: api.NewOptString("2026-06-21T11:00:00.0000000")}),
	}
	if _, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"}); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	for _, a := range backend.updatedEvent.Attendees {
		if a.Email == "bob@example.com" && a.Status != "notResponded" {
			t.Errorf("Bob status after significant change = %q, want notResponded (reset)", a.Status)
		}
	}
}

// Removing an attendee via PATCH sends them a METHOD:CANCEL and bumps SEQUENCE,
// without re-inviting the attendees who remain.
func TestMeUpdateEventsAttendeeRemovedCancels(t *testing.T) {
	ev := seededEvent
	ev.Attendees = []calendar.Attendee{{Email: "bob@example.com"}, {Email: "carol@example.com"}}
	backend := backendWithEvent(ev)
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	// PATCH keeps only Bob — Carol is dropped.
	req := &api.MicrosoftGraphEvent{Attendees: attendeeReq("bob@example.com")}
	if _, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"}); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse sent message: %v", err)
	}
	if inv.Method != scheduling.MethodCancel {
		t.Errorf("sent method = %q, want CANCEL", inv.Method)
	}
	if len(sender.to) != 1 || sender.to[0] != "carol@example.com" {
		t.Errorf("CANCEL to = %v, want [carol@example.com]", sender.to)
	}
	if backend.updatedEvent.Sequence != 1 {
		t.Errorf("SEQUENCE = %d, want 1 (bumped)", backend.updatedEvent.Sequence)
	}
}

// Adding an attendee re-sends a REQUEST (bumping SEQUENCE) but does NOT reset the
// existing attendees' responses — an add is not a significant change.
func TestMeUpdateEventsAttendeeAddedReinvitesWithoutReset(t *testing.T) {
	ev := seededEvent
	ev.Attendees = []calendar.Attendee{{Email: "bob@example.com", Status: "accepted"}}
	backend := backendWithEvent(ev)
	sender := &fakeSender{}
	h := Handler{
		calendar:   writableCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	req := &api.MicrosoftGraphEvent{Attendees: attendeeReq("bob@example.com", "carol@example.com")}
	if _, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"}); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse sent message: %v", err)
	}
	if inv.Method != scheduling.MethodRequest {
		t.Errorf("sent method = %q, want REQUEST", inv.Method)
	}
	if backend.updatedEvent.Sequence != 1 {
		t.Errorf("SEQUENCE = %d, want 1 (bumped)", backend.updatedEvent.Sequence)
	}
	for _, a := range backend.updatedEvent.Attendees {
		if a.Email == "bob@example.com" && a.Status != "accepted" {
			t.Errorf("Bob status after an attendee-add = %q, want accepted (not reset)", a.Status)
		}
	}
}

// A PATCH that restates a retained attendee WITH an explicit response must still
// preserve that attendee's stored SCHEDULE-STATUS (which has no Graph field, so the
// patch can never carry it) — guarding the mergeAttendees fix.
func TestMergeAttendeesPreservesScheduleStatusWithExplicitStatus(t *testing.T) {
	current := []calendar.Attendee{{Email: "bob@example.com", Status: "notResponded", ScheduleStatus: "1.1"}}
	patched := []calendar.Attendee{{Email: "bob@example.com", Status: "accepted"}} // explicit response, no ScheduleStatus
	got := mergeAttendees(current, patched)
	if len(got) != 1 {
		t.Fatalf("merged attendees = %d, want 1", len(got))
	}
	if got[0].Status != "accepted" {
		t.Errorf("Status = %q, want accepted (the patch's explicit value)", got[0].Status)
	}
	if got[0].ScheduleStatus != "1.1" {
		t.Errorf("ScheduleStatus = %q, want 1.1 (carried forward despite explicit Status)", got[0].ScheduleStatus)
	}
}

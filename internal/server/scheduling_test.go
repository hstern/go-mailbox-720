package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

type fakeSender struct {
	from    string
	to      []string
	raw     []byte
	closed  bool
	sendErr error // when set, Send records the attempt then returns this error
}

func (s *fakeSender) Send(_ context.Context, from string, to []string, raw []byte) error {
	s.from, s.to, s.raw = from, to, raw
	return s.sendErr
}
func (s *fakeSender) Close() error { s.closed = true; return nil }

type fakeSchedulingProvider struct {
	sender *fakeSender
	addr   string
}

func (p fakeSchedulingProvider) Sender(_ context.Context) (smtp.Sender, error) { return p.sender, nil }
func (p fakeSchedulingProvider) MailboxAddress(_ context.Context) (string, error) {
	return p.addr, nil
}

// seededInvite is a calendar backend whose GetEvent returns one event carrying a
// UID + organizer — what a reply needs.
func seededInvite() *fakeCalendarBackend {
	return &fakeCalendarBackend{
		events: map[string][]calendar.Event{"cal-primary": {{
			ID:        "evt-1",
			UID:       "uid-1@example.com",
			Subject:   "Standup",
			Start:     time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
			End:       time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC),
			Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
			Attendees: []calendar.Attendee{{Email: "me@example.com"}},
		}}},
	}
}

func TestMeEventsEventAcceptSendsReply(t *testing.T) {
	sender := &fakeSender{}
	h := Handler{
		calendar:   fakeCalendarProvider{backend: seededInvite()},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeEventsEventAccept(context.Background(), &api.MeEventsEventAcceptReq{}, api.MeEventsEventAcceptParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeEventsEventAccept: %v", err)
	}
	if _, ok := res.(*api.MeEventsEventAcceptNoContent); !ok {
		t.Fatalf("response = %T, want *MeEventsEventAcceptNoContent (204)", res)
	}
	if sender.from != "me@example.com" {
		t.Errorf("reply from = %q, want me@example.com", sender.from)
	}
	if len(sender.to) != 1 || sender.to[0] != "alice@example.com" {
		t.Errorf("reply to = %v, want [alice@example.com] (the organizer)", sender.to)
	}
	inv, err := scheduling.Parse(sender.raw)
	if err != nil {
		t.Fatalf("parse sent reply: %v", err)
	}
	if inv.Method != scheduling.MethodReply {
		t.Errorf("sent method = %q, want REPLY", inv.Method)
	}
	if inv.UID != "uid-1@example.com" {
		t.Errorf("sent UID = %q, want uid-1@example.com", inv.UID)
	}
	if !sender.closed {
		t.Error("sender not closed")
	}
}

func TestMeEventsEventAcceptSendResponseFalseSkips(t *testing.T) {
	sender := &fakeSender{}
	h := Handler{
		calendar:   fakeCalendarProvider{backend: seededInvite()},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	if _, err := h.MeEventsEventAccept(context.Background(),
		&api.MeEventsEventAcceptReq{SendResponse: api.NewOptNilBool(false)},
		api.MeEventsEventAcceptParams{EventID: "evt-1"}); err != nil {
		t.Fatalf("MeEventsEventAccept: %v", err)
	}
	if sender.from != "" {
		t.Error("a reply was sent despite sendResponse=false")
	}
}

func TestMeEventsEventDeclineNoMailboxAddress(t *testing.T) {
	sender := &fakeSender{}
	h := Handler{
		calendar:   fakeCalendarProvider{backend: seededInvite()},
		scheduling: fakeSchedulingProvider{sender: sender, addr: ""}, // unknown responder
	}
	res, err := h.MeEventsEventDecline(context.Background(), &api.MeEventsEventDeclineReq{}, api.MeEventsEventDeclineParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeEventsEventDecline: %v", err)
	}
	if errRes, ok := res.(*api.ErrorStatusCode); !ok || errRes.StatusCode != 400 {
		t.Errorf("response = %T, want a 400 ErrorStatusCode (no mailbox address)", res)
	}
}

func TestMeEventsEventTentativelyAcceptNilProviderNotImplemented(t *testing.T) {
	h := Handler{calendar: fakeCalendarProvider{backend: seededInvite()}} // no scheduling provider
	if _, err := h.MeEventsEventTentativelyAccept(context.Background(),
		&api.MeEventsEventTentativelyAcceptReq{}, api.MeEventsEventTentativelyAcceptParams{EventID: "evt-1"}); err == nil {
		t.Error("MeEventsEventTentativelyAccept with nil scheduling provider: expected error (501), got nil")
	}
}

// nativeCalendarBackend is a writable backend whose server schedules natively
// (RFC 6638). It records UpdateEvent via the embedded writable backend.
type nativeCalendarBackend struct {
	writableCalendarBackend
}

func (*nativeCalendarBackend) SupportsServerScheduling(context.Context) (bool, error) {
	return true, nil
}

type nativeCalendarProvider struct{ backend *nativeCalendarBackend }

func (p nativeCalendarProvider) Calendar(context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

func seededNativeInvite() *nativeCalendarBackend {
	return &nativeCalendarBackend{writableCalendarBackend: writableCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{events: map[string][]calendar.Event{"cal-primary": {{
			ID:        "evt-1",
			UID:       "uid-1@example.com",
			Subject:   "Standup",
			Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
			Attendees: []calendar.Attendee{{Email: "me@example.com", Status: "notResponded"}},
		}}}},
	}}
}

// When the CalDAV server schedules natively, accept records the responder's
// PARTSTAT via UpdateEvent (the server sends the reply) and does NOT email.
func TestMeEventsEventAcceptNativeUpdatesPartStat(t *testing.T) {
	sender := &fakeSender{}
	backend := seededNativeInvite()
	h := Handler{
		calendar:   nativeCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: sender, addr: "me@example.com"},
	}

	res, err := h.MeEventsEventAccept(context.Background(), &api.MeEventsEventAcceptReq{}, api.MeEventsEventAcceptParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeEventsEventAccept: %v", err)
	}
	if _, ok := res.(*api.MeEventsEventAcceptNoContent); !ok {
		t.Fatalf("response = %T, want 204", res)
	}
	if sender.from != "" {
		t.Error("emailed an iMIP reply even though the server schedules natively")
	}
	var status string
	for _, a := range backend.updatedEvent.Attendees {
		if a.Email == "me@example.com" {
			status = a.Status
		}
	}
	if status != "accepted" {
		t.Errorf("responder PARTSTAT in UpdateEvent = %q, want accepted (attendees: %+v)", status, backend.updatedEvent.Attendees)
	}
}

// When the responder is not an attendee, a native-scheduling response is a 400.
func TestMeEventsEventAcceptNativeNotAnAttendee(t *testing.T) {
	backend := seededNativeInvite()
	h := Handler{
		calendar:   nativeCalendarProvider{backend: backend},
		scheduling: fakeSchedulingProvider{sender: &fakeSender{}, addr: "stranger@example.com"},
	}
	res, err := h.MeEventsEventAccept(context.Background(), &api.MeEventsEventAcceptReq{}, api.MeEventsEventAcceptParams{EventID: "evt-1"})
	if err != nil {
		t.Fatalf("MeEventsEventAccept: %v", err)
	}
	if errRes, ok := res.(*api.ErrorStatusCode); !ok || errRes.StatusCode != 400 {
		t.Errorf("response = %T, want a 400 (mailbox not an attendee)", res)
	}
}

// TestMeEventsEventAcceptOmittedSendResponseSendsReply is the regression guard for
// the SendResponse spec-default fix: an empty {} body (omitted SendResponse) goes
// through the real ogen decode — unlike the Go-literal tests above — and must
// default to true and send the reply. With Graph's default:false left in place it
// would decode to false and silently skip the reply.
func TestMeEventsEventAcceptOmittedSendResponseSendsReply(t *testing.T) {
	sender := &fakeSender{}
	srv, err := New(nil, fakeCalendarProvider{backend: seededInvite()}, nil,
		fakeSchedulingProvider{sender: sender, addr: "me@example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1.0/me/events/evt-1/accept", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST accept: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if sender.from == "" {
		t.Error("omitted SendResponse did not send a reply — default:false regression")
	}
}

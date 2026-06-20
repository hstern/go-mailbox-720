package server

import (
	"context"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

type fakeSender struct {
	from   string
	to     []string
	raw    []byte
	closed bool
}

func (s *fakeSender) Send(_ context.Context, from string, to []string, raw []byte) error {
	s.from, s.to, s.raw = from, to, raw
	return nil
}
func (s *fakeSender) Close() error { s.closed = true; return nil }

type fakeSchedulingProvider struct {
	sender *fakeSender
	addr   string
}

func (p fakeSchedulingProvider) Sender(_ context.Context) (smtp.Sender, error) { return p.sender, nil }
func (p fakeSchedulingProvider) MailboxAddress() string                        { return p.addr }

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
			Attendees: []calendar.Address{{Email: "me@example.com"}},
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

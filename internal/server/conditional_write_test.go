package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// --- conditional fakes: each embeds the existing writable fake and adds the
// ConditionalWriter method, recording the If-Match it received and optionally
// failing the precondition. ---

type conditionalCalendarBackend struct {
	*writableCalendarBackend
	gotIfMatch       string
	conditionalCalls int
	failPrecondition bool
}

func (f *conditionalCalendarBackend) UpdateEventIfMatch(_ context.Context, e calendar.Event, ifMatch string) (calendar.Event, error) {
	f.gotIfMatch = ifMatch
	f.conditionalCalls++
	if f.failPrecondition {
		return calendar.Event{}, calendar.ErrPreconditionFailed
	}
	f.updatedEvent = e
	return e, nil
}

type conditionalCalendarProvider struct{ backend *conditionalCalendarBackend }

func (p conditionalCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

func newConditionalCalendarBackend() *conditionalCalendarBackend {
	return &conditionalCalendarBackend{writableCalendarBackend: newWritableCalendarBackendSeeded()}
}

type conditionalContactsBackend struct {
	*writableContactsBackend
	gotIfMatch       string
	conditionalCalls int
	failPrecondition bool
}

func (f *conditionalContactsBackend) UpdateContactIfMatch(_ context.Context, c contacts.Contact, ifMatch string) (contacts.Contact, error) {
	f.gotIfMatch = ifMatch
	f.conditionalCalls++
	if f.failPrecondition {
		return contacts.Contact{}, contacts.ErrPreconditionFailed
	}
	f.updatedContact = c
	return c, nil
}

type conditionalContactsProvider struct{ backend *conditionalContactsBackend }

func (p conditionalContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return p.backend, nil
}

func newConditionalContactsBackend() *conditionalContactsBackend {
	return &conditionalContactsBackend{writableContactsBackend: newWritableContactsBackendSeeded()}
}

type conditionalMailBackend struct {
	*writableMailBackend
	gotIfMatch       string
	conditionalCalls int
	failPrecondition bool
}

func (f *conditionalMailBackend) SetReadIfMatch(_ context.Context, id string, read bool, ifMatch string) error {
	f.gotIfMatch = ifMatch
	f.conditionalCalls++
	if f.failPrecondition {
		return mail.ErrPreconditionFailed
	}
	f.setReadID = id
	f.setReadValue = read
	f.message.IsRead = read
	return nil
}

type conditionalMailProvider struct{ backend *conditionalMailBackend }

func (p conditionalMailProvider) Mail(_ context.Context) (mail.Backend, error) {
	return p.backend, nil
}

func newConditionalMailBackend() *conditionalMailBackend {
	return &conditionalMailBackend{writableMailBackend: newWritableMailBackend()}
}

// --- events ---

func TestMeUpdateEventsConditionalThreadsIfMatch(t *testing.T) {
	backend := newConditionalCalendarBackend()
	h := Handler{calendar: conditionalCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("New Subject")}
	params := api.MeUpdateEventsParams{EventID: "evt-1", IfMatch: api.NewOptString(`"etag-1"`)}

	res, err := h.MeUpdateEvents(context.Background(), req, params)
	if err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if _, ok := res.(*api.MicrosoftGraphEventStatusCode); !ok {
		t.Fatalf("res type = %T, want 200 status code", res)
	}
	if backend.conditionalCalls != 1 {
		t.Errorf("conditional calls = %d, want 1 (should not use unconditional UpdateEvent)", backend.conditionalCalls)
	}
	if backend.gotIfMatch != `"etag-1"` {
		t.Errorf("If-Match threaded = %q, want %q", backend.gotIfMatch, `"etag-1"`)
	}
}

func TestMeUpdateEventsConditionalPreconditionFailed(t *testing.T) {
	backend := newConditionalCalendarBackend()
	backend.failPrecondition = true
	h := Handler{calendar: conditionalCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("New Subject")}
	params := api.MeUpdateEventsParams{EventID: "evt-1", IfMatch: api.NewOptString(`"stale"`)}

	_, err := h.MeUpdateEvents(context.Background(), req, params)
	if !errors.Is(err, calendar.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want calendar.ErrPreconditionFailed", err)
	}
}

func TestMeUpdateEventsNoIfMatchUsesUnconditional(t *testing.T) {
	backend := newConditionalCalendarBackend()
	h := Handler{calendar: conditionalCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("New Subject")}
	if _, err := h.MeUpdateEvents(context.Background(), req, api.MeUpdateEventsParams{EventID: "evt-1"}); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if backend.conditionalCalls != 0 {
		t.Errorf("conditional calls = %d, want 0 (no If-Match should use unconditional path)", backend.conditionalCalls)
	}
}

// A writable-but-not-conditional calendar backend (e.g. the JMAP calendar
// adapter) ignores If-Match and falls through to the unconditional update.
func TestMeUpdateEventsIfMatchOnNonConditionalBackendFallsThrough(t *testing.T) {
	backend := newWritableCalendarBackendSeeded() // Writer, NOT ConditionalWriter
	h := Handler{calendar: writableCalendarProvider{backend: backend}}

	req := &api.MicrosoftGraphEvent{Subject: api.NewOptNilString("New Subject")}
	params := api.MeUpdateEventsParams{EventID: "evt-1", IfMatch: api.NewOptString("ignored")}

	if _, err := h.MeUpdateEvents(context.Background(), req, params); err != nil {
		t.Fatalf("MeUpdateEvents: %v", err)
	}
	if backend.updatedEvent.ID == "" {
		t.Error("unconditional UpdateEvent not called (If-Match should be ignored on a non-conditional backend)")
	}
}

// --- contacts ---

func TestMeUpdateContactsConditionalThreadsIfMatch(t *testing.T) {
	backend := newConditionalContactsBackend()
	h := Handler{contacts: conditionalContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{DisplayName: api.NewOptNilString("New Name")}
	params := api.MeUpdateContactsParams{ContactID: "contact-1", IfMatch: api.NewOptString(`"etag-1"`)}

	if _, err := h.MeUpdateContacts(context.Background(), req, params); err != nil {
		t.Fatalf("MeUpdateContacts: %v", err)
	}
	if backend.conditionalCalls != 1 {
		t.Errorf("conditional calls = %d, want 1", backend.conditionalCalls)
	}
	if backend.gotIfMatch != `"etag-1"` {
		t.Errorf("If-Match threaded = %q, want %q", backend.gotIfMatch, `"etag-1"`)
	}
}

func TestMeUpdateContactsConditionalPreconditionFailed(t *testing.T) {
	backend := newConditionalContactsBackend()
	backend.failPrecondition = true
	h := Handler{contacts: conditionalContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{DisplayName: api.NewOptNilString("New Name")}
	params := api.MeUpdateContactsParams{ContactID: "contact-1", IfMatch: api.NewOptString(`"stale"`)}

	_, err := h.MeUpdateContacts(context.Background(), req, params)
	if !errors.Is(err, contacts.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want contacts.ErrPreconditionFailed", err)
	}
}

// A writable-but-not-conditional contacts backend (e.g. the JMAP contacts
// adapter) ignores If-Match and falls through to the unconditional update.
func TestMeUpdateContactsIfMatchOnNonConditionalBackendFallsThrough(t *testing.T) {
	backend := newWritableContactsBackendSeeded() // Writer, NOT ConditionalWriter
	h := Handler{contacts: writableContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{DisplayName: api.NewOptNilString("New Name")}
	params := api.MeUpdateContactsParams{ContactID: "contact-1", IfMatch: api.NewOptString("ignored")}

	if _, err := h.MeUpdateContacts(context.Background(), req, params); err != nil {
		t.Fatalf("MeUpdateContacts: %v", err)
	}
	if backend.updatedContact.ID == "" {
		t.Error("unconditional UpdateContact not called (If-Match should be ignored on a non-conditional backend)")
	}
}

// --- messages ---

func TestMeUpdateMessagesConditionalThreadsIfMatch(t *testing.T) {
	backend := newConditionalMailBackend()
	h := Handler{mail: conditionalMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(true)}
	params := api.MeUpdateMessagesParams{MessageID: "msg-1", IfMatch: api.NewOptString("s0")}

	if _, err := h.MeUpdateMessages(context.Background(), req, params); err != nil {
		t.Fatalf("MeUpdateMessages: %v", err)
	}
	if backend.conditionalCalls != 1 {
		t.Errorf("conditional calls = %d, want 1", backend.conditionalCalls)
	}
	if backend.gotIfMatch != "s0" {
		t.Errorf("If-Match threaded = %q, want %q", backend.gotIfMatch, "s0")
	}
}

func TestMeUpdateMessagesConditionalPreconditionFailed(t *testing.T) {
	backend := newConditionalMailBackend()
	backend.failPrecondition = true
	h := Handler{mail: conditionalMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(true)}
	params := api.MeUpdateMessagesParams{MessageID: "msg-1", IfMatch: api.NewOptString("stale")}

	_, err := h.MeUpdateMessages(context.Background(), req, params)
	if !errors.Is(err, mail.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want mail.ErrPreconditionFailed", err)
	}
}

// A writable-but-not-conditional mail backend (the IMAP case) ignores If-Match
// and falls through to the unconditional read-state write.
func TestMeUpdateMessagesIfMatchOnNonConditionalBackendFallsThrough(t *testing.T) {
	backend := newWritableMailBackend() // Writer, NOT ConditionalWriter
	h := Handler{mail: writableMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(true)}
	params := api.MeUpdateMessagesParams{MessageID: "msg-1", IfMatch: api.NewOptString("ignored")}

	if _, err := h.MeUpdateMessages(context.Background(), req, params); err != nil {
		t.Fatalf("MeUpdateMessages: %v", err)
	}
	if backend.setReadCalls != 1 {
		t.Errorf("unconditional SetRead calls = %d, want 1 (If-Match ignored on non-conditional backend)", backend.setReadCalls)
	}
}

// --- error handler: each port's precondition sentinel maps to 412 ---

func TestGraphErrorHandlerMapsPreconditionFailed(t *testing.T) {
	for _, err := range []error{
		mail.ErrPreconditionFailed,
		calendar.ErrPreconditionFailed,
		contacts.ErrPreconditionFailed,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/me/events/evt-1", nil)
		graphErrorHandler(context.Background(), rec, req, err)
		if rec.Code != http.StatusPreconditionFailed {
			t.Errorf("status for %v = %d, want %d", err, rec.Code, http.StatusPreconditionFailed)
		}
	}
}

// --- @odata.etag surfacing on reads ---

func TestToGraphSurfacesODataEtag(t *testing.T) {
	gm := toGraphMessage(mail.Message{ID: "m", ETag: `"m-etag"`})
	if v, _ := gm.OdataDotEtag.Get(); v != `"m-etag"` {
		t.Errorf("message @odata.etag = %q, want %q", v, `"m-etag"`)
	}

	ev := calendar.Event{ID: "e", ETag: `"e-etag"`}
	if v, _ := toGraphEvent(ev).OdataDotEtag.Get(); v != `"e-etag"` {
		t.Errorf("event @odata.etag = %q, want %q", v, `"e-etag"`)
	}

	ct := contacts.Contact{ID: "c", ETag: `"c-etag"`}
	if v, _ := toGraphContact(ct).OdataDotEtag.Get(); v != `"c-etag"` {
		t.Errorf("contact @odata.etag = %q, want %q", v, `"c-etag"`)
	}

	// No ETag → annotation absent (not an empty-string field).
	if toGraphMessage(mail.Message{ID: "m"}).OdataDotEtag.Set {
		t.Error("message @odata.etag set with no ETag; want absent")
	}
}

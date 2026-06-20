package server

import (
	"context"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// fakeMailBackend is an in-memory mail.Backend over a single message, used by the
// write-handler tests. It is read-only (no Writer); writableMailBackend embeds it
// to add the write capability.
type fakeMailBackend struct {
	message mail.Message
	closed  bool
}

func (f *fakeMailBackend) ListMailFolders(_ context.Context) ([]mail.MailFolder, error) {
	return nil, nil
}

func (f *fakeMailBackend) ListMessages(_ context.Context, _ string, _ mail.Page, _ *odata.Filter) ([]mail.Message, error) {
	return []mail.Message{f.message}, nil
}

func (f *fakeMailBackend) GetMessage(_ context.Context, _ string) (mail.Message, error) {
	return f.message, nil
}

func (f *fakeMailBackend) Close() error {
	f.closed = true
	return nil
}

// fakeMailProvider hands out a read-only fakeMailBackend.
type fakeMailProvider struct {
	backend *fakeMailBackend
}

func (p fakeMailProvider) Mail(_ context.Context) (mail.Backend, error) {
	return p.backend, nil
}

// writableMailBackend implements BOTH mail.Backend and mail.Writer, recording the
// SetRead/DeleteMessage calls so tests can assert the handler reached the Writer
// with the right arguments.
type writableMailBackend struct {
	fakeMailBackend

	setReadID    string
	setReadValue bool
	setReadCalls int
	deletedID    string
}

func (f *writableMailBackend) SetRead(_ context.Context, id string, read bool) error {
	f.setReadID = id
	f.setReadValue = read
	f.setReadCalls++
	f.message.IsRead = read
	return nil
}

func (f *writableMailBackend) DeleteMessage(_ context.Context, id string) error {
	f.deletedID = id
	return nil
}

// writableMailProvider hands out a writableMailBackend (Backend+Writer).
type writableMailProvider struct {
	backend *writableMailBackend
}

func (p writableMailProvider) Mail(_ context.Context) (mail.Backend, error) {
	return p.backend, nil
}

func newWritableMailBackend() *writableMailBackend {
	return &writableMailBackend{
		fakeMailBackend: fakeMailBackend{
			message: mail.Message{ID: "msg-1", Subject: "Hello", IsRead: false},
		},
	}
}

func TestMeUpdateMessagesSetReadTrue(t *testing.T) {
	backend := newWritableMailBackend()
	h := Handler{mail: writableMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(true)}
	res, err := h.MeUpdateMessages(context.Background(), req, api.MeUpdateMessagesParams{MessageID: "msg-1"})
	if err != nil {
		t.Fatalf("MeUpdateMessages: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphMessageStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphMessageStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if backend.setReadCalls != 1 {
		t.Fatalf("SetRead call count = %d, want 1", backend.setReadCalls)
	}
	if backend.setReadID != "msg-1" {
		t.Errorf("SetRead id = %q, want msg-1", backend.setReadID)
	}
	if backend.setReadValue != true {
		t.Errorf("SetRead read = %v, want true", backend.setReadValue)
	}
	if got := ok.Response.IsRead.Or(false); got != true {
		t.Errorf("returned isRead = %v, want true", got)
	}
	if !backend.closed {
		t.Error("backend not closed")
	}
}

func TestMeUpdateMessagesSetReadFalse(t *testing.T) {
	backend := newWritableMailBackend()
	backend.message.IsRead = true
	h := Handler{mail: writableMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(false)}
	if _, err := h.MeUpdateMessages(context.Background(), req, api.MeUpdateMessagesParams{MessageID: "msg-1"}); err != nil {
		t.Fatalf("MeUpdateMessages: %v", err)
	}
	if backend.setReadCalls != 1 {
		t.Fatalf("SetRead call count = %d, want 1", backend.setReadCalls)
	}
	if backend.setReadValue != false {
		t.Errorf("SetRead read = %v, want false", backend.setReadValue)
	}
}

// A PATCH body that omits isRead must not call SetRead at all (only the explicit
// fields in the body are honoured).
func TestMeUpdateMessagesNoIsReadSkipsSetRead(t *testing.T) {
	backend := newWritableMailBackend()
	h := Handler{mail: writableMailProvider{backend: backend}}

	req := &api.MicrosoftGraphMessage{Subject: api.NewOptNilString("ignored")}
	if _, err := h.MeUpdateMessages(context.Background(), req, api.MeUpdateMessagesParams{MessageID: "msg-1"}); err != nil {
		t.Fatalf("MeUpdateMessages: %v", err)
	}
	if backend.setReadCalls != 0 {
		t.Errorf("SetRead call count = %d, want 0 (isRead absent)", backend.setReadCalls)
	}
}

func TestMeDeleteMessagesCallsWriter(t *testing.T) {
	backend := newWritableMailBackend()
	h := Handler{mail: writableMailProvider{backend: backend}}

	res, err := h.MeDeleteMessages(context.Background(), api.MeDeleteMessagesParams{MessageID: "msg-1"})
	if err != nil {
		t.Fatalf("MeDeleteMessages: %v", err)
	}
	if _, ok := res.(*api.MeDeleteMessagesNoContent); !ok {
		t.Fatalf("response type = %T, want *MeDeleteMessagesNoContent (204)", res)
	}
	if backend.deletedID != "msg-1" {
		t.Errorf("deleted id = %q, want msg-1", backend.deletedID)
	}
}

// A read-only backend (Backend but not Writer) must yield the not-implemented
// sentinel (rendered as a Graph 501) for both PATCH and delete.
func TestMeUpdateMessagesReadOnlyBackendNotImplemented(t *testing.T) {
	backend := &fakeMailBackend{message: mail.Message{ID: "msg-1"}}
	h := Handler{mail: fakeMailProvider{backend: backend}}

	if _, err := h.MeUpdateMessages(context.Background(), &api.MicrosoftGraphMessage{IsRead: api.NewOptNilBool(true)}, api.MeUpdateMessagesParams{MessageID: "msg-1"}); err == nil {
		t.Error("MeUpdateMessages on read-only backend: expected error, got nil")
	}
	if _, err := h.MeDeleteMessages(context.Background(), api.MeDeleteMessagesParams{MessageID: "msg-1"}); err == nil {
		t.Error("MeDeleteMessages on read-only backend: expected error, got nil")
	}
}

func TestNilMailProviderWriteNotImplemented(t *testing.T) {
	h := Handler{}
	if _, err := h.MeUpdateMessages(context.Background(), &api.MicrosoftGraphMessage{}, api.MeUpdateMessagesParams{MessageID: "x"}); err == nil {
		t.Error("MeUpdateMessages with nil provider: expected error, got nil")
	}
	if _, err := h.MeDeleteMessages(context.Background(), api.MeDeleteMessagesParams{MessageID: "x"}); err == nil {
		t.Error("MeDeleteMessages with nil provider: expected error, got nil")
	}
}

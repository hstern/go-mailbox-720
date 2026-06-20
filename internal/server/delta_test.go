package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// deltaMailBackend implements mail.Backend + mail.DeltaReader, recording the
// token the handler passes and returning canned delta results.
type deltaMailBackend struct {
	fakeMailBackend
	gotToken string
	msgs     []mail.Message
	next     string
	err      error
}

func (f *deltaMailBackend) Delta(_ context.Context, _ string, token string) ([]mail.Message, string, error) {
	f.gotToken = token
	if f.err != nil {
		return nil, "", f.err
	}
	return f.msgs, f.next, nil
}

type deltaMailProvider struct{ backend *deltaMailBackend }

func (p deltaMailProvider) Mail(_ context.Context) (mail.Backend, error) { return p.backend, nil }

func TestMeMessagesDelta(t *testing.T) {
	backend := &deltaMailBackend{
		msgs: []mail.Message{{Subject: "Hi"}},
		next: "TOKEN-2",
	}
	h := Handler{mail: deltaMailProvider{backend: backend}}

	res, err := h.MeMessagesDelta(context.Background(), api.MeMessagesDeltaParams{
		Deltatoken: api.NewOptString("TOKEN-1"),
	})
	if err != nil {
		t.Fatalf("MeMessagesDelta: %v", err)
	}
	ok, isOK := res.(*api.MeMessagesDelta2XXStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MeMessagesDelta2XXStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if backend.gotToken != "TOKEN-1" {
		t.Errorf("backend received token %q, want TOKEN-1", backend.gotToken)
	}
	if len(ok.Response.Value) != 1 || ok.Response.Value[0].Subject.Or("") != "Hi" {
		t.Errorf("value = %+v, want one message 'Hi'", ok.Response.Value)
	}
	if link := ok.Response.OdataDotDeltaLink.Or(""); !strings.Contains(link, "$deltatoken=TOKEN-2") {
		t.Errorf("deltaLink = %q, want it to carry the next token", link)
	}
}

// A backend that does not implement DeltaReader yields not-implemented (501).
func TestMeMessagesDeltaReadOnlyNotImplemented(t *testing.T) {
	h := Handler{mail: fakeMailProvider{backend: &fakeMailBackend{}}}
	if _, err := h.MeMessagesDelta(context.Background(), api.MeMessagesDeltaParams{}); err == nil {
		t.Error("MeMessagesDelta on a non-DeltaReader backend: expected error, got nil")
	}
}

// An invalid continuation token (mail.ErrInvalidDeltaToken) maps to a Graph 410
// resyncRequired so the client restarts with an initial sync, not a 500.
func TestMeMessagesDeltaInvalidTokenResync(t *testing.T) {
	backend := &deltaMailBackend{err: fmt.Errorf("decode: %w", mail.ErrInvalidDeltaToken)}
	h := Handler{mail: deltaMailProvider{backend: backend}}

	res, err := h.MeMessagesDelta(context.Background(), api.MeMessagesDeltaParams{
		Deltatoken: api.NewOptString("garbage!!!"),
	})
	if err != nil {
		t.Fatalf("MeMessagesDelta: %v", err)
	}
	errRes, ok := res.(*api.ErrorStatusCode)
	if !ok {
		t.Fatalf("response type = %T, want *ErrorStatusCode", res)
	}
	if errRes.StatusCode != 410 {
		t.Errorf("status = %d, want 410 (resyncRequired)", errRes.StatusCode)
	}
}

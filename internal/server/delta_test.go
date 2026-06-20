package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// deltaMailBackend implements mail.Backend + mail.DeltaReader, recording the
// token the handler passes and returning canned delta results.
type deltaMailBackend struct {
	fakeMailBackend
	gotToken string
	msgs     []mail.Message
	removed  []string
	next     string
	err      error
}

func (f *deltaMailBackend) Delta(_ context.Context, _ string, token string) ([]mail.Message, []string, string, error) {
	f.gotToken = token
	if f.err != nil {
		return nil, nil, "", f.err
	}
	return f.msgs, f.removed, f.next, nil
}

type deltaMailProvider struct{ backend *deltaMailBackend }

func (p deltaMailProvider) Mail(_ context.Context) (mail.Backend, error) { return p.backend, nil }

func TestMessagesDeltaHandlerTombstones(t *testing.T) {
	backend := &deltaMailBackend{
		msgs:    []mail.Message{{Subject: "Hi"}},
		removed: []string{"msg-gone"},
		next:    "TOKEN-2",
	}
	h := MessagesDeltaHandler(deltaMailProvider{backend: backend})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/messages/delta()?$deltatoken=TOKEN-1", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if backend.gotToken != "TOKEN-1" {
		t.Errorf("backend received token %q, want TOKEN-1", backend.gotToken)
	}
	var resp deltaResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Value) != 2 {
		t.Fatalf("value length = %d, want 2 (one changed + one tombstone): %s", len(resp.Value), w.Body.String())
	}
	if resp.Value[0]["subject"] != "Hi" {
		t.Errorf("changed item = %v, want subject Hi", resp.Value[0])
	}
	tomb := resp.Value[1]
	if tomb["id"] != "msg-gone" {
		t.Errorf("tombstone id = %v, want msg-gone", tomb["id"])
	}
	if removed, ok := tomb["@removed"].(map[string]any); !ok || removed["reason"] != "deleted" {
		t.Errorf("tombstone @removed = %v, want {reason: deleted}", tomb["@removed"])
	}
	if !strings.Contains(resp.DeltaLink, "$deltatoken=TOKEN-2") {
		t.Errorf("deltaLink = %q, want it to carry the next token", resp.DeltaLink)
	}
}

func TestMessagesDeltaHandlerNotImplemented(t *testing.T) {
	// A backend that is not a DeltaReader yields 501.
	h := MessagesDeltaHandler(fakeMailProvider{backend: &fakeMailBackend{}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/messages/delta()", nil))
	if w.Code != 501 {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestMessagesDeltaHandlerInvalidTokenResync(t *testing.T) {
	// An invalid continuation token maps to 410 so the client restarts sync.
	backend := &deltaMailBackend{err: fmt.Errorf("decode: %w", mail.ErrInvalidDeltaToken)}
	h := MessagesDeltaHandler(deltaMailProvider{backend: backend})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/messages/delta()?$deltatoken=garbage", nil))
	if w.Code != 410 {
		t.Errorf("status = %d, want 410 (resync)", w.Code)
	}
}

func TestMessagesDeltaHandlerUnsupported(t *testing.T) {
	// A backend without QRESYNC delta support maps to 501.
	backend := &deltaMailBackend{err: mail.ErrDeltaUnsupported}
	h := MessagesDeltaHandler(deltaMailProvider{backend: backend})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/messages/delta()", nil))
	if w.Code != 501 {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

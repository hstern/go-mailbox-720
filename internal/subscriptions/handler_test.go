package subscriptions

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const handlerMaxTTL = 72 * time.Hour

var handlerNow = time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)

// echoServer returns an httptest TLS server that performs the Graph validation
// handshake correctly: it echoes the validationToken back as text/plain 200. It
// is TLS (https) because Validate requires an https notificationUrl; the
// returned server's Client() trusts the test certificate.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("validationToken")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(token))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wrongTokenServer returns an https server that 200s but echoes the wrong body,
// so the handshake must fail.
func wrongTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-the-token"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestHandler builds a handler over a fresh MemoryStore, using client for the
// notificationUrl handshake and a fixed clock.
func newTestHandler(t *testing.T, store *MemoryStore, client *http.Client) http.Handler {
	t.Helper()
	return NewHandler(store, client, allowedResources, handlerMaxTTL, func() time.Time { return handlerNow })
}

// createBody renders a POST body, defaulting the fields to a valid subscription
// whose notificationUrl is url.
func createBody(url string, overrides map[string]any) []byte {
	body := map[string]any{
		"changeType":         "created,updated",
		"notificationUrl":    url,
		"resource":           "/me/messages",
		"expirationDateTime": handlerNow.Add(24 * time.Hour).Format(time.RFC3339),
		"clientState":        "opaque-secret",
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return b
}

// decodeError pulls {"error":{"code","message"}} out of a response body.
func decodeError(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not a Graph error object: %v (body=%s)", err, body)
	}
	if env.Error.Code == "" {
		t.Fatalf("response has no error.code: %s", body)
	}
	return env.Error.Code, env.Error.Message
}

func TestHandlerStampsOwnerAndNotifiesWatch(t *testing.T) {
	srv := echoServer(t)
	store := NewMemoryStore()
	h := NewHandler(store, srv.Client(), allowedResources, handlerMaxTTL, func() time.Time { return handlerNow })
	h.SetOwnerFunc(func(*http.Request) string { return "iss|alice" })

	var gotOwner string
	var calls int
	h.SetOnSubscribe(func(_ *http.Request, owner string) { calls++; gotOwner = owner })

	req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(createBody(srv.URL, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body)
	}
	subs := store.List()
	if len(subs) != 1 || subs[0].Owner != "iss|alice" {
		t.Fatalf("stored owner = %q, want iss|alice (subs=%+v)", func() string {
			if len(subs) > 0 {
				return subs[0].Owner
			}
			return ""
		}(), subs)
	}
	if calls != 1 || gotOwner != "iss|alice" {
		t.Fatalf("onSubscribe calls=%d owner=%q, want 1 and iss|alice", calls, gotOwner)
	}
}

func TestHandlerCreateValid(t *testing.T) {
	srv := echoServer(t)
	store := NewMemoryStore()
	h := newTestHandler(t, store, srv.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(createBody(srv.URL, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body)
	}

	var got subscriptionWire
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID == "" {
		t.Error("response has no id")
	}
	if got.Resource != "/me/messages" {
		t.Errorf("resource = %q, want /me/messages", got.Resource)
	}
	if got.ChangeType != "created,updated" {
		t.Errorf("changeType = %q, want created,updated", got.ChangeType)
	}
	if got.NotificationURL != srv.URL {
		t.Errorf("notificationUrl = %q, want %q", got.NotificationURL, srv.URL)
	}
	// Graph echoes clientState back on create.
	if got.ClientState != "opaque-secret" {
		t.Errorf("clientState = %q, want opaque-secret", got.ClientState)
	}
	if _, err := time.Parse(time.RFC3339, got.ExpirationDateTime); err != nil {
		t.Errorf("expirationDateTime %q is not RFC3339: %v", got.ExpirationDateTime, err)
	}

	// It was actually stored.
	if list := store.List(); len(list) != 1 {
		t.Fatalf("store has %d subscriptions, want 1", len(list))
	} else if list[0].ID != got.ID {
		t.Errorf("stored id = %q, want %q", list[0].ID, got.ID)
	}
}

func TestHandlerCreateRejections(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
	}{
		{"unsupported resource", map[string]any{"resource": "/me/drive"}},
		{"non-https url", map[string]any{"notificationUrl": "http://app.example.com/notify"}},
		{"empty changeType", map[string]any{"changeType": ""}},
		{"unknown changeType", map[string]any{"changeType": "created,exploded"}},
		{"expiration in the past", map[string]any{"expirationDateTime": handlerNow.Add(-time.Hour).Format(time.RFC3339)}},
		{"expiration too far", map[string]any{"expirationDateTime": handlerNow.Add(handlerMaxTTL + time.Hour).Format(time.RFC3339)}},
		{"missing expiration", map[string]any{"expirationDateTime": nil}},
		{"malformed expiration", map[string]any{"expirationDateTime": "not-a-date"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A real echo server is wired so a 400 can only come from validation,
			// never from the handshake. The non-https case never reaches it.
			srv := echoServer(t)
			store := NewMemoryStore()
			h := newTestHandler(t, store, srv.Client())

			// For the non-https case the body's notificationUrl must override the
			// echo server URL; otherwise default to the echo server.
			body := createBody(srv.URL, tc.overrides)
			req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body)
			}
			code, _ := decodeError(t, rec.Body.Bytes())
			if code == "" {
				t.Errorf("missing Graph error code")
			}
			if n := len(store.List()); n != 0 {
				t.Errorf("store has %d subscriptions, want 0 (nothing stored on rejection)", n)
			}
		})
	}
}

func TestHandlerCreateHandshakeFails(t *testing.T) {
	srv := wrongTokenServer(t)
	store := NewMemoryStore()
	h := newTestHandler(t, store, srv.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(createBody(srv.URL, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body)
	}
	decodeError(t, rec.Body.Bytes())
	if n := len(store.List()); n != 0 {
		t.Errorf("store has %d subscriptions, want 0 (handshake failed, nothing stored)", n)
	}
}

func TestHandlerCreateBadJSON(t *testing.T) {
	srv := echoServer(t)
	store := NewMemoryStore()
	h := newTestHandler(t, store, srv.Client())

	req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body)
	}
	decodeError(t, rec.Body.Bytes())
	if n := len(store.List()); n != 0 {
		t.Errorf("store has %d subscriptions, want 0", n)
	}
}

func TestHandlerList(t *testing.T) {
	srv := echoServer(t)
	store := NewMemoryStore()
	h := newTestHandler(t, store, srv.Client())

	// Create two through the handler.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(createBody(srv.URL, nil)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d status = %d, want 201 (body=%s)", i, rec.Code, rec.Body)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1.0/subscriptions", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
	}
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(env.Value) != 2 {
		t.Fatalf("list has %d subscriptions, want 2", len(env.Value))
	}
	for _, s := range env.Value {
		if s.ID == "" {
			t.Error("listed subscription has no id")
		}
	}
}

func TestHandlerListEmpty(t *testing.T) {
	store := NewMemoryStore()
	h := newTestHandler(t, store, nil) // no handshake needed for GET

	req := httptest.NewRequest(http.MethodGet, "/v1.0/subscriptions", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// An empty list must be {"value":[]}, not {"value":null}.
	if got := strings.TrimSpace(rec.Body.String()); got != `{"value":[]}` {
		t.Errorf("body = %s, want {\"value\":[]}", got)
	}
}

func TestHandlerDeleteExisting(t *testing.T) {
	store := NewMemoryStore()
	h := newTestHandler(t, store, nil)

	created, err := store.Create(Subscription{
		Resource:           "/me/messages",
		ChangeType:         "created",
		NotificationURL:    "https://app.example.com/notify",
		ExpirationDateTime: handlerNow.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1.0/subscriptions/"+created.ID, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 body = %q, want empty", rec.Body)
	}
	if n := len(store.List()); n != 0 {
		t.Errorf("store has %d subscriptions after delete, want 0", n)
	}
}

func TestHandlerDeleteAbsent(t *testing.T) {
	store := NewMemoryStore()
	h := newTestHandler(t, store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/v1.0/subscriptions/does-not-exist", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != "notFound" {
		t.Errorf("error code = %q, want notFound", code)
	}
}

func TestHandlerDeleteNoID(t *testing.T) {
	store := NewMemoryStore()
	h := newTestHandler(t, store, nil)

	// DELETE on the collection path (no id) is a 404.
	req := httptest.NewRequest(http.MethodDelete, "/v1.0/subscriptions", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body)
	}
	decodeError(t, rec.Body.Bytes())
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	store := NewMemoryStore()
	h := newTestHandler(t, store, nil)

	req := httptest.NewRequest(http.MethodPut, "/v1.0/subscriptions", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body=%s)", rec.Code, rec.Body)
	}
	code, _ := decodeError(t, rec.Body.Bytes())
	if code != "methodNotAllowed" {
		t.Errorf("error code = %q, want methodNotAllowed", code)
	}
}

func TestHandlerDefaultNow(t *testing.T) {
	// A nil now must default to time.Now (rather than panic): an expiration in the
	// near future relative to real time should validate.
	srv := echoServer(t)
	store := NewMemoryStore()
	h := NewHandler(store, srv.Client(), allowedResources, handlerMaxTTL, nil)

	body := map[string]any{
		"changeType":         "created",
		"notificationUrl":    srv.URL,
		"resource":           "/me/messages",
		"expirationDateTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"clientState":        "s",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1.0/subscriptions", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body)
	}
}

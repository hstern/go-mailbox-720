package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var allowedResources = []string{"/me/messages", "/me/events", "/me/contacts"}

// baseReq returns a valid subscription request relative to now, which individual
// table cases mutate to trip one rejection.
func baseReq(now time.Time) Subscription {
	return Subscription{
		Resource:           "/me/messages",
		ChangeType:         "created,updated",
		NotificationURL:    "https://app.example.com/notify",
		ClientState:        "secret",
		ExpirationDateTime: now.Add(24 * time.Hour),
	}
}

func TestValidate(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	const maxTTL = 72 * time.Hour

	tests := []struct {
		name    string
		mutate  func(*Subscription)
		wantErr error // nil means happy path
	}{
		{
			name:   "valid",
			mutate: func(*Subscription) {},
		},
		{
			name:    "empty notificationUrl",
			mutate:  func(s *Subscription) { s.NotificationURL = "" },
			wantErr: ErrNotificationURLRequired,
		},
		{
			name:    "non-https notificationUrl",
			mutate:  func(s *Subscription) { s.NotificationURL = "http://app.example.com/notify" },
			wantErr: ErrNotificationURLNotHTTPS,
		},
		{
			name:    "empty changeType",
			mutate:  func(s *Subscription) { s.ChangeType = "" },
			wantErr: ErrInvalidChangeType,
		},
		{
			name:    "unknown changeType",
			mutate:  func(s *Subscription) { s.ChangeType = "created,exploded" },
			wantErr: ErrInvalidChangeType,
		},
		{
			name:    "unsupported resource",
			mutate:  func(s *Subscription) { s.Resource = "/me/drive" },
			wantErr: ErrUnsupportedResource,
		},
		{
			name:    "expiration in the past",
			mutate:  func(s *Subscription) { s.ExpirationDateTime = now.Add(-time.Minute) },
			wantErr: ErrExpirationInPast,
		},
		{
			name:    "expiration equal to now",
			mutate:  func(s *Subscription) { s.ExpirationDateTime = now },
			wantErr: ErrExpirationInPast,
		},
		{
			name:    "expiration beyond maxTTL",
			mutate:  func(s *Subscription) { s.ExpirationDateTime = now.Add(maxTTL + time.Hour) },
			wantErr: ErrExpirationTooFar,
		},
		{
			name:   "expiration exactly at maxTTL boundary",
			mutate: func(s *Subscription) { s.ExpirationDateTime = now.Add(maxTTL) },
		},
		{
			name:   "single changeType",
			mutate: func(s *Subscription) { s.ChangeType = "deleted" },
		},
		{
			name:   "resource match is case-insensitive",
			mutate: func(s *Subscription) { s.Resource = "/Me/Messages" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq(now)
			tc.mutate(&req)
			err := Validate(req, now, maxTTL, allowedResources)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyNotificationURL(t *testing.T) {
	t.Run("success echoes token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			token := r.URL.Query().Get("validationToken")
			if token == "" {
				t.Error("validationToken query param missing")
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(token))
		}))
		defer srv.Close()

		if err := VerifyNotificationURL(context.Background(), srv.Client(), srv.URL); err != nil {
			t.Fatalf("VerifyNotificationURL() = %v, want nil", err)
		}
	})

	t.Run("wrong body fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-the-token"))
		}))
		defer srv.Close()

		err := VerifyNotificationURL(context.Background(), srv.Client(), srv.URL)
		if err == nil {
			t.Fatal("VerifyNotificationURL() = nil, want error")
		}
	})

	t.Run("non-200 fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		if err := VerifyNotificationURL(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Fatal("VerifyNotificationURL() = nil, want error")
		}
	})

	t.Run("redirect is refused", func(t *testing.T) {
		// A redirect must not be followed: otherwise an https URL could 302 to an
		// internal http target and bypass the scheme gate. Even though the redirect
		// target here would happily echo the token, the handshake must fail.
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			token := r.URL.Query().Get("validationToken")
			if r.URL.Path == "/echo" {
				_, _ = w.Write([]byte(token))
				return
			}
			http.Redirect(w, r, "/echo?validationToken="+token, http.StatusFound)
		}))
		defer srv.Close()

		if err := VerifyNotificationURL(context.Background(), srv.Client(), srv.URL); err == nil {
			t.Fatal("VerifyNotificationURL() = nil, want error (redirect must not be followed)")
		}
		if hits != 1 {
			t.Errorf("server hit %d times, want 1 (redirect must not be followed)", hits)
		}
	})

	t.Run("timeout fails", func(t *testing.T) {
		// The handler blocks until the request context is cancelled; with an
		// already-expired context the handshake must fail promptly.
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already cancelled
		if err := VerifyNotificationURL(ctx, srv.Client(), srv.URL); err == nil {
			t.Fatal("VerifyNotificationURL() = nil, want error")
		}
	})

	t.Run("nil client fails", func(t *testing.T) {
		if err := VerifyNotificationURL(context.Background(), nil, "https://example.com"); err == nil {
			t.Fatal("VerifyNotificationURL(nil client) = nil, want error")
		}
	})
}

func TestNotificationPayload(t *testing.T) {
	sub := Subscription{
		ID:          "sub-123",
		Resource:    "/me/messages",
		ChangeType:  "created",
		ClientState: "secret-state",
	}

	b, err := NotificationPayload(sub, "AAMkADk")
	if err != nil {
		t.Fatalf("NotificationPayload() error = %v", err)
	}

	var env notificationEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(env.Value) != 1 {
		t.Fatalf("value length = %d, want 1", len(env.Value))
	}
	n := env.Value[0]
	if n.SubscriptionID != "sub-123" {
		t.Errorf("subscriptionId = %q, want sub-123", n.SubscriptionID)
	}
	if n.ClientState != "secret-state" {
		t.Errorf("clientState = %q, want secret-state", n.ClientState)
	}
	if n.ChangeType != ChangeCreated {
		t.Errorf("changeType = %q, want created", n.ChangeType)
	}
	if want := "/me/messages/AAMkADk"; n.Resource != want {
		t.Errorf("resource = %q, want %q", n.Resource, want)
	}
	if n.ResourceData.ID != "AAMkADk" {
		t.Errorf("resourceData.id = %q, want AAMkADk", n.ResourceData.ID)
	}
	if n.ResourceData.ODataID != "/me/messages/AAMkADk" {
		t.Errorf("resourceData.@odata.id = %q", n.ResourceData.ODataID)
	}

	// Verify the literal @odata.* JSON keys are present (struct tags carry dots).
	if !strings.Contains(string(b), `"@odata.id"`) || !strings.Contains(string(b), `"@odata.type"`) {
		t.Errorf("payload missing @odata keys: %s", b)
	}
}

func TestNotificationPayloadTrimsTrailingSlash(t *testing.T) {
	sub := Subscription{ID: "s", Resource: "/me/messages/", ChangeType: "updated"}
	b, err := NotificationPayload(sub, "id1")
	if err != nil {
		t.Fatalf("NotificationPayload() error = %v", err)
	}
	var env notificationEnvelope
	_ = json.Unmarshal(b, &env)
	if want := "/me/messages/id1"; env.Value[0].Resource != want {
		t.Errorf("resource = %q, want %q", env.Value[0].Resource, want)
	}
}

func TestMemoryStoreCRUD(t *testing.T) {
	s := NewMemoryStore()
	fixed := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	exp := fixed.Add(24 * time.Hour)
	created, err := s.Create(Subscription{Resource: "/me/messages", ExpirationDateTime: exp})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID == "" {
		t.Fatal("Create() did not assign an ID")
	}
	if !created.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, fixed)
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("Get id = %q, want %q", got.ID, created.ID)
	}

	if list := s.List(); len(list) != 1 {
		t.Fatalf("List() len = %d, want 1", len(list))
	}

	if err := s.Delete(created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := s.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing = %v, want ErrNotFound", err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreDuplicateID(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.Create(Subscription{ID: "fixed"}); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if _, err := s.Create(Subscription{ID: "fixed"}); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("duplicate Create() = %v, want ErrDuplicateID", err)
	}
}

func TestMemoryStoreListOrdered(t *testing.T) {
	s := NewMemoryStore()
	t0 := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"c", "a", "b"} {
		s.now = func() time.Time { return t0.Add(time.Duration(i) * time.Minute) }
		if _, err := s.Create(Subscription{ID: id}); err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
	}
	list := s.List()
	gotIDs := []string{list[0].ID, list[1].ID, list[2].ID}
	want := []string{"c", "a", "b"} // ordered by CreatedAt, which increases per insert
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Errorf("List order = %v, want %v", gotIDs, want)
			break
		}
	}
}

func TestMemoryStoreDeleteExpired(t *testing.T) {
	s := NewMemoryStore()
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)

	mustCreate := func(id string, exp time.Time) {
		if _, err := s.Create(Subscription{ID: id, ExpirationDateTime: exp}); err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
	}
	mustCreate("expired", now.Add(-time.Hour))
	mustCreate("at-now", now) // at-or-before now counts as expired
	mustCreate("future", now.Add(time.Hour))

	removed := s.DeleteExpired(now)
	if removed != 2 {
		t.Errorf("DeleteExpired removed = %d, want 2", removed)
	}
	if list := s.List(); len(list) != 1 || list[0].ID != "future" {
		t.Errorf("after DeleteExpired list = %v, want only future", list)
	}
}

func TestMemoryStoreConcurrent(t *testing.T) {
	s := NewMemoryStore()
	const goroutines = 16
	const perGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				sub, err := s.Create(Subscription{Resource: "/me/messages"})
				if err != nil {
					t.Errorf("Create() error = %v", err)
					return
				}
				if _, err := s.Get(sub.ID); err != nil {
					t.Errorf("Get() error = %v", err)
					return
				}
				_ = s.List()
			}
		}()
	}
	wg.Wait()

	if got := len(s.List()); got != goroutines*perGoroutine {
		t.Errorf("final count = %d, want %d", got, goroutines*perGoroutine)
	}
}

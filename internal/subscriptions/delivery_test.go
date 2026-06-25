package subscriptions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// recorder is an httptest server that records the bodies POSTed to it and
// responds with a configurable status, used as a subscription's notificationUrl.
type recorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

// newRecorder starts a recorder that replies with status to every POST.
func newRecorder(t *testing.T, status int) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, body)
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.bodies)
}

func (rec *recorder) lastBody(t *testing.T) []byte {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 {
		t.Fatal("recorder received no POSTs")
	}
	return rec.bodies[len(rec.bodies)-1]
}

func (rec *recorder) lastEnvelope(t *testing.T) notificationEnvelope {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 {
		t.Fatal("recorder received no POSTs")
	}
	var env notificationEnvelope
	if err := json.Unmarshal(rec.bodies[len(rec.bodies)-1], &env); err != nil {
		t.Fatalf("unmarshal recorded body: %v", err)
	}
	return env
}

func TestNotify(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	// One recorder per subscription so we can assert exactly who got a POST.
	matchA := newRecorder(t, http.StatusAccepted)             // matching, 2xx
	matchB := newRecorder(t, http.StatusOK)                   // matching, 2xx
	failing := newRecorder(t, http.StatusInternalServerError) // matching but 500
	wrongResource := newRecorder(t, http.StatusOK)            // resource mismatch
	wrongChange := newRecorder(t, http.StatusOK)              // changeType mismatch
	expired := newRecorder(t, http.StatusOK)                  // expired

	store := NewMemoryStore()
	mustCreate := func(sub Subscription) {
		t.Helper()
		if _, err := store.Create(sub); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	mustCreate(Subscription{ID: "match-a", Resource: "/me/messages", ChangeType: "created,updated", NotificationURL: matchA.srv.URL, ClientState: "state-a", ExpirationDateTime: future})
	mustCreate(Subscription{ID: "match-b", Resource: "/me/messages", ChangeType: "updated", NotificationURL: matchB.srv.URL, ClientState: "state-b", ExpirationDateTime: future})
	mustCreate(Subscription{ID: "failing", Resource: "/me/messages", ChangeType: "updated", NotificationURL: failing.srv.URL, ClientState: "state-f", ExpirationDateTime: future})
	mustCreate(Subscription{ID: "wrong-resource", Resource: "/me/events", ChangeType: "updated", NotificationURL: wrongResource.srv.URL, ExpirationDateTime: future})
	mustCreate(Subscription{ID: "wrong-change", Resource: "/me/messages", ChangeType: "created", NotificationURL: wrongChange.srv.URL, ExpirationDateTime: future})
	mustCreate(Subscription{ID: "expired", Resource: "/me/messages", ChangeType: "updated", NotificationURL: expired.srv.URL, ExpirationDateTime: past})

	change := Change{
		Resource:    "/me/messages",
		ChangeType:  ChangeUpdated,
		ResourceIDs: []string{"id1", "id2"},
	}

	result := Notify(context.Background(), http.DefaultClient, store, change, now)

	// Three subscriptions match (match-a, match-b, failing); two deliver (matchA,
	// matchB); the failing one is a recorded error but did not abort the rest.
	if result.Matched != 3 {
		t.Errorf("Matched = %d, want 3", result.Matched)
	}
	if result.Delivered != 2 {
		t.Errorf("Delivered = %d, want 2", result.Delivered)
	}
	if len(result.Errors) != 1 {
		t.Errorf("Errors len = %d, want 1 (%v)", len(result.Errors), result.Errors)
	}
	if _, ok := result.Errors["failing"]; !ok {
		t.Errorf("Errors missing the failing subscription: %v", result.Errors)
	}

	// Only the matching, unexpired subscriptions received a POST.
	if got := matchA.count(); got != 1 {
		t.Errorf("matchA POSTs = %d, want 1", got)
	}
	if got := matchB.count(); got != 1 {
		t.Errorf("matchB POSTs = %d, want 1", got)
	}
	if got := failing.count(); got != 1 {
		t.Errorf("failing POSTs = %d, want 1 (delivery attempted)", got)
	}
	if got := wrongResource.count(); got != 0 {
		t.Errorf("wrongResource POSTs = %d, want 0", got)
	}
	if got := wrongChange.count(); got != 0 {
		t.Errorf("wrongChange POSTs = %d, want 0", got)
	}
	if got := expired.count(); got != 0 {
		t.Errorf("expired POSTs = %d, want 0", got)
	}

	// The matching subscription's body is a batched {value:[...]} with both ids,
	// the right clientState, changeType, and resource paths.
	env := matchA.lastEnvelope(t)
	if len(env.Value) != 2 {
		t.Fatalf("batched value length = %d, want 2", len(env.Value))
	}
	gotResources := []string{env.Value[0].Resource, env.Value[1].Resource}
	sort.Strings(gotResources)
	wantResources := []string{"/me/messages/id1", "/me/messages/id2"}
	for i := range wantResources {
		if gotResources[i] != wantResources[i] {
			t.Errorf("batched resources = %v, want %v", gotResources, wantResources)
			break
		}
	}
	for _, n := range env.Value {
		if n.SubscriptionID != "match-a" {
			t.Errorf("subscriptionId = %q, want match-a", n.SubscriptionID)
		}
		if n.ClientState != "state-a" {
			t.Errorf("clientState = %q, want state-a", n.ClientState)
		}
		if n.ChangeType != ChangeUpdated && n.ChangeType != "created,updated" {
			t.Errorf("changeType = %q, unexpected", n.ChangeType)
		}
	}
}

func TestNotifyNoMatches(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	rec := newRecorder(t, http.StatusOK)
	if _, err := store.Create(Subscription{ID: "s", Resource: "/me/events", ChangeType: "updated", NotificationURL: rec.srv.URL, ExpirationDateTime: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	result := Notify(context.Background(), http.DefaultClient, store, Change{
		Resource:    "/me/messages",
		ChangeType:  ChangeUpdated,
		ResourceIDs: []string{"id1"},
	}, now)

	if result.Matched != 0 || result.Delivered != 0 || len(result.Errors) != 0 {
		t.Errorf("Result = %+v, want all zero", result)
	}
	if got := rec.count(); got != 0 {
		t.Errorf("recorder POSTs = %d, want 0", got)
	}
}

func TestNotifyScopesByOwner(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)

	alice := newRecorder(t, http.StatusOK)
	bob := newRecorder(t, http.StatusOK)
	unowned := newRecorder(t, http.StatusOK)

	store := NewMemoryStore()
	for _, s := range []Subscription{
		{ID: "alice", Owner: "iss|alice", Resource: "/me/messages", ChangeType: "updated", NotificationURL: alice.srv.URL, ExpirationDateTime: future},
		{ID: "bob", Owner: "iss|bob", Resource: "/me/messages", ChangeType: "updated", NotificationURL: bob.srv.URL, ExpirationDateTime: future},
		{ID: "unowned", Resource: "/me/messages", ChangeType: "updated", NotificationURL: unowned.srv.URL, ExpirationDateTime: future},
	} {
		if _, err := store.Create(s); err != nil {
			t.Fatalf("Create(%s): %v", s.ID, err)
		}
	}

	// A change owned by alice reaches only alice's subscription — not bob's, not
	// the unowned one.
	res := Notify(context.Background(), http.DefaultClient, store, Change{
		Owner: "iss|alice", Resource: "/me/messages", ChangeType: ChangeUpdated, ResourceIDs: []string{"m1"},
	}, now)
	if res.Matched != 1 || res.Delivered != 1 {
		t.Fatalf("alice change: Matched=%d Delivered=%d, want 1/1", res.Matched, res.Delivered)
	}
	if alice.count() != 1 || bob.count() != 0 || unowned.count() != 0 {
		t.Fatalf("POST counts alice=%d bob=%d unowned=%d, want 1/0/0", alice.count(), bob.count(), unowned.count())
	}

	// An unowned (single-tenant) change reaches only the unowned subscription.
	res = Notify(context.Background(), http.DefaultClient, store, Change{
		Resource: "/me/messages", ChangeType: ChangeUpdated, ResourceIDs: []string{"m2"},
	}, now)
	if res.Matched != 1 || unowned.count() != 1 || alice.count() != 1 {
		t.Fatalf("unowned change: Matched=%d unowned=%d alice=%d, want 1, unowned+1, alice unchanged", res.Matched, unowned.count(), alice.count())
	}
}

func TestNotifyResourceMatchCaseInsensitive(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	rec := newRecorder(t, http.StatusOK)
	if _, err := store.Create(Subscription{ID: "s", Resource: "/Me/Messages", ChangeType: "updated", NotificationURL: rec.srv.URL, ExpirationDateTime: now.Add(time.Hour)}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	result := Notify(context.Background(), http.DefaultClient, store, Change{
		Resource:    "/me/messages",
		ChangeType:  ChangeUpdated,
		ResourceIDs: []string{"id1"},
	}, now)

	if result.Matched != 1 || result.Delivered != 1 {
		t.Errorf("Result = %+v, want 1 matched/delivered", result)
	}
}

func TestNotificationPayloadBatch(t *testing.T) {
	sub := Subscription{ID: "sub-1", Resource: "/me/messages/", ChangeType: "created", ClientState: "secret"}

	b, err := NotificationPayloadBatch(sub, []string{"a", "b"})
	if err != nil {
		t.Fatalf("NotificationPayloadBatch() error = %v", err)
	}
	var env notificationEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Value) != 2 {
		t.Fatalf("value length = %d, want 2", len(env.Value))
	}
	if env.Value[0].Resource != "/me/messages/a" || env.Value[1].Resource != "/me/messages/b" {
		t.Errorf("resources = %q,%q, want trimmed slash join", env.Value[0].Resource, env.Value[1].Resource)
	}
	for _, n := range env.Value {
		if n.SubscriptionID != "sub-1" || n.ClientState != "secret" || n.ChangeType != ChangeCreated {
			t.Errorf("entry mismatch: %+v", n)
		}
	}
}

func TestNotificationPayloadBatchEmpty(t *testing.T) {
	b, err := NotificationPayloadBatch(Subscription{ID: "s", Resource: "/me/messages"}, nil)
	if err != nil {
		t.Fatalf("NotificationPayloadBatch() error = %v", err)
	}
	var env notificationEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Value) != 0 {
		t.Errorf("value length = %d, want 0", len(env.Value))
	}
}

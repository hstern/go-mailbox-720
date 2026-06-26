package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNotifyLifecycleEmitsToOwnersWithLifecycleURL(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)

	withURL := newRecorder(t, http.StatusOK)
	store := NewMemoryStore()
	// alice has a lifecycle URL; bob (other owner) and a no-lifecycle sub must
	// not receive it.
	for _, s := range []Subscription{
		{Owner: "alice", Resource: "/me/messages", ChangeType: "updated", NotificationURL: "https://data.example", LifecycleNotificationURL: withURL.srv.URL, ClientState: "cs", ExpirationDateTime: future},
		{Owner: "alice", Resource: "/me/events", ChangeType: "updated", NotificationURL: "https://data.example", ExpirationDateTime: future}, // no lifecycle URL
		{Owner: "bob", Resource: "/me/messages", ChangeType: "updated", NotificationURL: "https://data.example", LifecycleNotificationURL: newRecorder(t, http.StatusOK).srv.URL, ExpirationDateTime: future},
	} {
		if _, err := store.Create(s); err != nil {
			t.Fatal(err)
		}
	}

	res := NotifyLifecycle(context.Background(), http.DefaultClient, store, "alice", LifecycleReauthorizationRequired, now)
	if res.Matched != 1 || res.Delivered != 1 || len(res.Errors) != 0 {
		t.Fatalf("Result = %+v, want Matched=1 Delivered=1 no errors", res)
	}
	if withURL.count() != 1 {
		t.Fatalf("lifecycle POSTs = %d, want 1", withURL.count())
	}

	var env struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			LifecycleEvent string `json:"lifecycleEvent"`
			ClientState    string `json:"clientState"`
		} `json:"value"`
	}
	if err := json.Unmarshal(withURL.lastBody(t), &env); err != nil {
		t.Fatalf("decode lifecycle body: %v", err)
	}
	if len(env.Value) != 1 || env.Value[0].LifecycleEvent != "reauthorizationRequired" {
		t.Fatalf("lifecycle payload = %+v, want one reauthorizationRequired", env.Value)
	}
	if env.Value[0].ClientState != "cs" {
		t.Errorf("clientState = %q, want cs", env.Value[0].ClientState)
	}
}

func TestNotifyLifecycleSkipsExpired(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	rec := newRecorder(t, http.StatusOK)
	store := NewMemoryStore()
	if _, err := store.Create(Subscription{
		Owner: "alice", Resource: "/me/messages", ChangeType: "updated",
		NotificationURL: "https://data.example", LifecycleNotificationURL: rec.srv.URL,
		ExpirationDateTime: now.Add(-time.Minute), // already expired
	}); err != nil {
		t.Fatal(err)
	}
	res := NotifyLifecycle(context.Background(), http.DefaultClient, store, "alice", LifecycleMissed, now)
	if res.Matched != 0 || rec.count() != 0 {
		t.Fatalf("expired subscription got a lifecycle POST: Matched=%d posts=%d", res.Matched, rec.count())
	}
}

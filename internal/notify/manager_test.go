package notify

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

var (
	mgrNow    = time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	mgrFuture = mgrNow.Add(24 * time.Hour)
)

// bodyRecorder records the notification POST bodies it receives.
type bodyRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	got    chan []byte
}

func newBodyRecorder(t *testing.T) *bodyRecorder {
	t.Helper()
	rec := &bodyRecorder{got: make(chan []byte, 8)}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, b)
		rec.mu.Unlock()
		rec.got <- b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

// fixedBuilder returns a ResourceBuilder whose watch fires onChange once then
// blocks on ctx, and whose sync returns ids on the first non-baseline call.
// onClosed, if non-nil, is closed when the watch's context ends (so a test can
// observe teardown).
func fixedBuilder(resource string, ids []string, expiresAt time.Time, onClosed chan struct{}) ResourceBuilder {
	return ResourceBuilder{
		Resource: resource,
		Build: func(ctx context.Context, token string) (WatchFunc, SyncFunc, time.Time, bool, error) {
			watch := func(ctx context.Context, onChange func()) error {
				onChange()
				<-ctx.Done()
				if onClosed != nil {
					close(onClosed)
				}
				return nil
			}
			var calls int
			sync := func(ctx context.Context, token string) ([]string, string, error) {
				calls++
				if calls == 1 {
					return nil, "t1", nil // baseline
				}
				return ids, "t2", nil
			}
			return watch, sync, expiresAt, true, nil
		},
	}
}

func TestManagerDeliversForPrincipalOnChange(t *testing.T) {
	rec := newBodyRecorder(t)
	store := subscriptions.NewMemoryStore()
	if _, err := store.Create(subscriptions.Subscription{
		Owner: "alice", Resource: EventsResource, ChangeType: "created",
		NotificationURL: rec.srv.URL, ExpirationDateTime: mgrFuture,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, []ResourceBuilder{fixedBuilder(EventsResource, []string{"evt-1"}, mgrFuture, nil)},
		store, rec.srv.Client(), func() time.Time { return mgrNow }, nil)

	m.OnSubscribe("alice", "tok")

	select {
	case b := <-rec.got:
		if !bytes.Contains(b, []byte("evt-1")) {
			t.Fatalf("notification body = %s, want it to reference evt-1", b)
		}
		if !bytes.Contains(b, []byte(EventsResource)) {
			t.Fatalf("notification body = %s, want %s", b, EventsResource)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not deliver a notification for the principal")
	}
}

func TestManagerNoWatchWithoutSubscriptions(t *testing.T) {
	rec := newBodyRecorder(t)
	store := subscriptions.NewMemoryStore() // no subscriptions for bob
	built := false
	b := ResourceBuilder{Resource: EventsResource, Build: func(ctx context.Context, token string) (WatchFunc, SyncFunc, time.Time, bool, error) {
		built = true
		return nil, nil, time.Time{}, false, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, []ResourceBuilder{b}, store, rec.srv.Client(), func() time.Time { return mgrNow }, nil)

	m.OnSubscribe("bob", "tok")
	if built {
		t.Fatal("manager built a watch for a principal with no subscriptions")
	}
}

func TestManagerIgnoresEmptyOwnerOrToken(t *testing.T) {
	store := subscriptions.NewMemoryStore()
	called := false
	b := ResourceBuilder{Resource: EventsResource, Build: func(ctx context.Context, token string) (WatchFunc, SyncFunc, time.Time, bool, error) {
		called = true
		return nil, nil, time.Time{}, false, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, []ResourceBuilder{b}, store, http.DefaultClient, func() time.Time { return mgrNow }, nil)

	m.OnSubscribe("", "tok")
	m.OnSubscribe("alice", "")
	if called {
		t.Fatal("manager built a watch for an empty owner or token")
	}
}

func TestManagerReapStopsWatchWhenSubscriptionsExpire(t *testing.T) {
	rec := newBodyRecorder(t)
	store := subscriptions.NewMemoryStore()
	shortExp := mgrNow.Add(time.Minute)
	if _, err := store.Create(subscriptions.Subscription{
		Owner: "alice", Resource: EventsResource, ChangeType: "created",
		NotificationURL: rec.srv.URL, ExpirationDateTime: shortExp,
	}); err != nil {
		t.Fatal(err)
	}

	now := mgrNow
	closed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(ctx, []ResourceBuilder{fixedBuilder(EventsResource, []string{"evt-1"}, mgrFuture, closed)},
		store, rec.srv.Client(), func() time.Time { return now }, nil)

	m.OnSubscribe("alice", "tok")
	<-rec.got // watch is live

	// Advance time past the subscription's expiry and reap: the watch must stop.
	now = shortExp.Add(time.Second)
	m.Reap()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Reap did not stop the watch after the subscription expired")
	}
}

package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

// fakeWatcher fires onChange a fixed number of times (synchronously) then blocks
// until ctx is cancelled, modelling an IDLE watch.
type fakeWatcher struct{ fires int }

func (w *fakeWatcher) Watch(ctx context.Context, _ string, onChange func()) error {
	for i := 0; i < w.fires; i++ {
		onChange()
	}
	<-ctx.Done()
	return nil
}

// fakeSyncer returns no messages on the first (baseline) Delta call and a fixed
// set on every subsequent call, advancing the token each time.
type fakeSyncer struct {
	mu       sync.Mutex
	calls    int
	baseline []mail.Message
	msgs     []mail.Message
}

func (s *fakeSyncer) Delta(_ context.Context, _, _ string) ([]mail.Message, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return s.baseline, "tok1", nil
	}
	return s.msgs, fmt.Sprintf("tok%d", s.calls), nil
}

func newSub(t *testing.T, store subscriptions.Store, url string) {
	t.Helper()
	if _, err := store.Create(subscriptions.Subscription{
		Resource:           MessagesResource,
		ChangeType:         subscriptions.ChangeCreated,
		NotificationURL:    url,
		ClientState:        "secret",
		ExpirationDateTime: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

func TestRunDeliversOnInboxChange(t *testing.T) {
	bodies := make(chan []byte, 4)
	recv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	store := subscriptions.NewMemoryStore()
	newSub(t, store, recv.URL)

	// A pre-existing message is returned by the baseline sync and must NOT be
	// notified; only the message that arrives via onChange is.
	syncer := &fakeSyncer{
		baseline: []mail.Message{{ID: "old"}},
		msgs:     []mail.Message{{ID: "new-1"}},
	}
	watcher := &fakeWatcher{fires: 1}

	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan subscriptions.Result, 4)
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, watcher, syncer, store, recv.Client(), time.Now, func(r subscriptions.Result) { reports <- r })
		close(done)
	}()

	select {
	case r := <-reports:
		if r.Matched != 1 || r.Delivered != 1 || len(r.Errors) != 0 {
			t.Errorf("Result = %+v, want Matched=1 Delivered=1 no errors", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery reported")
	}

	select {
	case b := <-bodies:
		if !bytes.Contains(b, []byte("new-1")) {
			t.Errorf("notification body = %s, want it to reference new-1", b)
		}
		if bytes.Contains(b, []byte(`"old"`)) {
			t.Errorf("notification body referenced the pre-existing message: %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification POST received")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// A baseline sync failure aborts Run before watching.
func TestRunBaselineError(t *testing.T) {
	w := &fakeWatcher{fires: 1}
	s := &errSyncer{}
	if err := Run(context.Background(), w, s, subscriptions.NewMemoryStore(), http.DefaultClient, time.Now, nil); err == nil {
		t.Error("Run() = nil, want error from baseline sync")
	}
}

type errSyncer struct{}

func (errSyncer) Delta(_ context.Context, _, _ string) ([]mail.Message, string, error) {
	return nil, "", fmt.Errorf("boom")
}

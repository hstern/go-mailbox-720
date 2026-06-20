//go:build dockertest

// Integration test for the change-notification DELIVERY loop (internal/notify)
// against a real Dovecot server in Docker. It drives notify.Run directly with two
// real *imap.Client connections (one for the IDLE watch, one for delta sync) and
// a permissive http client, so that an appended message flows all the way to a
// POSTed Graph change notification at a 127.0.0.1 webhook.
//
// It lives in package imap (not internal/notify) so it can reuse the Dovecot
// helpers in dovecot_test.go (startDovecot/appendToINBOX/dovecotUser/...) and dial
// real Client values. There is no import cycle: internal/notify imports
// internal/mail, not internal/mail/imap, and a test file in package imap may
// import internal/notify and internal/subscriptions.
//
// Build-tagged so the default `go test ./...` stays fast and dependency-free; run
// with:
//
//	go test -tags dockertest ./internal/mail/imap/
//
// Self-skips when docker is unavailable.
package imap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/notify"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

// TestDovecotNotifyDelivery exercises the full delivery loop end to end: a real
// Dovecot inbox, the IMAP IDLE watcher + delta reader wired through notify.Run,
// and a webhook receiver that must get exactly one Graph change-notification POST
// when a message arrives.
func TestDovecotNotifyDelivery(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	addr := startDovecot(t)

	// Two SEPARATE connections: the IDLE watch monopolizes its session, so the
	// delta sync must run on its own. Both implement mail.Watcher and
	// mail.DeltaReader (a *imap.Client satisfies both).
	watchClient, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial (watch): %v", err)
	}
	t.Cleanup(func() { _ = watchClient.Close() })

	syncClient, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial (sync): %v", err)
	}
	t.Cleanup(func() { _ = syncClient.Close() })

	// Webhook receiver: record each POST body and return 200. The channel is
	// buffered so the receiver never blocks even if the test is slow to read.
	bodies := make(chan []byte, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		// Only record real change-notification deliveries: subscriptions.deliver
		// POSTs a JSON body. Ignore stray probes (e.g. a GET / health check from
		// some other local process hitting the freshly-opened port).
		if r.Method != http.MethodPost || len(body) == 0 {
			return
		}
		bodies <- body
	}))
	t.Cleanup(srv.Close)

	// One subscription watching /me/messages for "created", delivering to the
	// webhook. Created directly via the store, bypassing the handler/validation
	// (which would reject the http 127.0.0.1 URL).
	store := subscriptions.NewMemoryStore()
	if _, err := store.Create(subscriptions.Subscription{
		Resource:           notify.MessagesResource,
		ChangeType:         subscriptions.ChangeCreated,
		NotificationURL:    srv.URL,
		ClientState:        "secret",
		ExpirationDateTime: time.Now().Add(72 * time.Hour),
	}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drive the loop directly with a permissive client (httptest's, which dials
	// 127.0.0.1) — production's GuardedClient would refuse the loopback webhook.
	runErr := make(chan error, 1)
	go func() {
		runErr <- notify.Run(ctx, watchClient, syncClient, store, srv.Client(), time.Now, nil)
	}()

	// CRITICAL timing: notify.Run takes a BASELINE delta and then enters IDLE. If
	// we append before the watch is established, the baseline captures the new
	// message and the change is never delivered. Give the baseline + IDLE a
	// generous moment to settle before appending.
	time.Sleep(1500 * time.Millisecond)

	newRaw := "From: Carol <carol@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Notify me\r\n" +
		"Date: Thu, 12 Jun 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"A new arrival that should be delivered.\r\n"
	appendToINBOX(t, addr, newRaw)

	// Within a few seconds the webhook must receive a POST.
	var body []byte
	select {
	case body = <-bodies:
	case <-time.After(10 * time.Second):
		t.Fatal("webhook did not receive a notification within 10s of an APPEND")
	}

	// Assert the envelope shape, clientState, and resource path. Message ids are
	// opaque, so we do not assert on them.
	var env struct {
		Value []struct {
			ClientState string `json:"clientState"`
			ChangeType  string `json:"changeType"`
			Resource    string `json:"resource"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("notification body is not a JSON envelope: %v\nbody: %s", err, body)
	}
	if len(env.Value) != 1 {
		t.Fatalf("envelope has %d entries, want 1: %s", len(env.Value), body)
	}
	got := env.Value[0]
	if got.ClientState != "secret" {
		t.Errorf("clientState = %q, want %q", got.ClientState, "secret")
	}
	if got.ChangeType != string(subscriptions.ChangeCreated) {
		t.Errorf("changeType = %q, want %q", got.ChangeType, subscriptions.ChangeCreated)
	}
	if want := notify.MessagesResource + "/"; len(got.Resource) <= len(want) || got.Resource[:len(want)] != want {
		t.Errorf("resource = %q, want prefix %q with an id appended", got.Resource, want)
	}

	// No second delivery should arrive for this single append.
	select {
	case extra := <-bodies:
		t.Errorf("received an unexpected second notification: %s", extra)
	case <-time.After(500 * time.Millisecond):
	}

	// Cancel and drain the loop: Run must return (nil) once ctx is cancelled.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("notify.Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("notify.Run did not return within 10s of ctx cancel")
	}
}

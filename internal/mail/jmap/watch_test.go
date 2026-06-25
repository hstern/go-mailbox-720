package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"github.com/coder/websocket"

	"github.com/hstern/go-mailbox-720/internal/jmap/push"
)

// watchTestClient builds a Client whose session advertises a WebSocket push
// endpoint at wsURL. supportsPush controls the capability flag.
func watchTestClient(wsURL string, supportsPush bool) *Client {
	body, _ := json.Marshal(push.WebSocketCapability{URL: wsURL, SupportsPush: supportsPush})
	gc := &gojmap.Client{Session: &gojmap.Session{
		RawCapabilities: map[gojmap.URI]json.RawMessage{push.CapabilityURI: body},
	}}
	cl := newClient(gc, testAccount)
	cl.token = "tok"
	return cl
}

// emailPushServer starts a JMAP-over-WebSocket server that, once the client
// enables push, writes one Email StateChange and then blocks until the client
// disconnects.
func emailPushServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"jmap"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		if _, _, err := c.Read(ctx); err != nil { // WebSocketPushEnable
			return
		}
		sc := gojmap.StateChange{
			Type:    "StateChange",
			Changed: map[gojmap.ID]gojmap.TypeState{testAccount: {dataTypeEmail: "s2"}},
		}
		b, _ := json.Marshal(sc)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			return
		}
		<-ctx.Done()
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestWatchDeliversEmailChange(t *testing.T) {
	cl := watchTestClient(emailPushServer(t), true)

	signal := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- cl.Watch(ctx, "", func() { signal <- struct{}{} }) }()

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange never fired for an Email StateChange")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Watch returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}
}

func TestWatchWithoutWebSocketCapability(t *testing.T) {
	// A session with no websocket capability: Watch must report it rather than
	// hang or silently no-op.
	gc := &gojmap.Client{Session: &gojmap.Session{RawCapabilities: map[gojmap.URI]json.RawMessage{}}}
	cl := newClient(gc, testAccount)
	cl.token = "tok"

	err := cl.Watch(context.Background(), "", func() {})
	if err == nil {
		t.Fatal("Watch returned nil when the server advertised no WebSocket push")
	}
}

func TestWatchWithoutPushSupport(t *testing.T) {
	// Capability present but supportsPush=false: push would never arrive, so
	// Watch must refuse rather than connect and block forever.
	cl := watchTestClient("ws://unused.example/ws", false)
	if err := cl.Watch(context.Background(), "", func() {}); err == nil {
		t.Fatal("Watch returned nil when supportsPush=false")
	}
}

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

// calWatchClient builds a calendar Client whose session advertises a WebSocket
// push endpoint at wsURL, authenticated with token.
func calWatchClient(wsURL string, supportsPush bool, token string) *Client {
	body, _ := json.Marshal(push.WebSocketCapability{URL: wsURL, SupportsPush: supportsPush})
	gc := &gojmap.Client{Session: &gojmap.Session{
		RawCapabilities: map[gojmap.URI]json.RawMessage{push.CapabilityURI: body},
	}}
	return &Client{c: gc, accountID: "acc1", token: token}
}

// eventPushServer starts a JMAP-over-WebSocket server that, once push is enabled,
// writes one CalendarEvent StateChange and then blocks until the client leaves.
func eventPushServer(t *testing.T) string {
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
			Changed: map[gojmap.ID]gojmap.TypeState{"acc1": {dataTypeCalendarEvent: "s2"}},
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

func TestWatchDeliversCalendarEventChange(t *testing.T) {
	cl := calWatchClient(eventPushServer(t), true, "tok")

	signal := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- cl.Watch(ctx, "", func() { signal <- struct{}{} }) }()

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange never fired for a CalendarEvent StateChange")
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

func TestWatchCalendarWithoutWebSocketCapability(t *testing.T) {
	gc := &gojmap.Client{Session: &gojmap.Session{RawCapabilities: map[gojmap.URI]json.RawMessage{}}}
	cl := &Client{c: gc, accountID: "acc1", token: "tok"}
	if err := cl.Watch(context.Background(), "", func() {}); err == nil {
		t.Fatal("Watch returned nil when the server advertised no WebSocket push")
	}
}

func TestWatchCalendarRequiresBearer(t *testing.T) {
	// A session authenticated with HTTP Basic auth has no bearer token; the
	// bearer-only push consumer cannot watch, so Watch must refuse.
	cl := calWatchClient("ws://unused.example/ws", true, "")
	if err := cl.Watch(context.Background(), "", func() {}); err == nil {
		t.Fatal("Watch returned nil for a Basic-auth session (empty token)")
	}
}

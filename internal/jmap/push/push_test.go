package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"github.com/coder/websocket"
)

// pushEnableSeen captures the WebSocketPushEnable the client sends, so a test can
// assert which data types the consumer asked the server to push.
type pushEnableSeen struct {
	Type      string   `json:"@type"`
	DataTypes []string `json:"dataTypes"`
}

// wsURL rewrites an httptest http:// URL to ws:// so the dial reads as a real
// WebSocket endpoint (coder/websocket accepts either, but ws:// matches prod).
func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

// newPushServer starts a JMAP-over-WebSocket test server. It negotiates the
// "jmap" subprotocol, records the client's WebSocketPushEnable on enabled, then
// writes every StateChange received on send to the socket until the client
// disconnects. authWant, when non-empty, is the Authorization header it requires.
func newPushServer(t *testing.T, authWant string, enabled chan<- pushEnableSeen, send <-chan gojmap.StateChange) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authWant != "" && r.Header.Get("Authorization") != authWant {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"jmap"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		if c.Subprotocol() != "jmap" {
			c.Close(websocket.StatusPolicyViolation, "expected jmap subprotocol")
			return
		}
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var pe pushEnableSeen
		if err := json.Unmarshal(data, &pe); err != nil {
			return
		}
		select {
		case enabled <- pe:
		case <-ctx.Done():
			return
		}
		for {
			select {
			case sc, ok := <-send:
				if !ok {
					return
				}
				b, _ := json.Marshal(sc)
				if err := c.Write(ctx, websocket.MessageText, b); err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stateChange(account string, types map[string]string) gojmap.StateChange {
	ts := gojmap.TypeState{}
	for k, v := range types {
		ts[k] = v
	}
	return gojmap.StateChange{
		Type:    "StateChange",
		Changed: map[gojmap.ID]gojmap.TypeState{gojmap.ID(account): ts},
	}
}

func TestConsumerDispatchesStateChangeByType(t *testing.T) {
	enabled := make(chan pushEnableSeen, 1)
	send := make(chan gojmap.StateChange)
	srv := newPushServer(t, "Bearer tok", enabled, send)

	var mu sync.Mutex
	got := map[string]int{}
	signal := make(chan string, 8)
	mark := func(name string) func() {
		return func() {
			mu.Lock()
			got[name]++
			mu.Unlock()
			signal <- name
		}
	}

	c := New(wsURL(srv.URL), "tok", nil)
	c.Subscribe("Email", mark("Email"))
	c.Subscribe("CalendarEvent", mark("CalendarEvent"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	// The consumer must advertise both subscribed data types in PushEnable.
	select {
	case pe := <-enabled:
		if pe.Type != "WebSocketPushEnable" {
			t.Fatalf("first client message @type = %q, want WebSocketPushEnable", pe.Type)
		}
		if !contains(pe.DataTypes, "Email") || !contains(pe.DataTypes, "CalendarEvent") {
			t.Fatalf("PushEnable dataTypes = %v, want both Email and CalendarEvent", pe.DataTypes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received WebSocketPushEnable")
	}

	// An Email StateChange fires only the Email handler.
	send <- stateChange("acc1", map[string]string{"Email": "s2"})
	if name := waitSignal(t, signal); name != "Email" {
		t.Fatalf("first signal = %q, want Email", name)
	}

	// A CalendarEvent StateChange fires only the CalendarEvent handler.
	send <- stateChange("acc1", map[string]string{"CalendarEvent": "c9"})
	if name := waitSignal(t, signal); name != "CalendarEvent" {
		t.Fatalf("second signal = %q, want CalendarEvent", name)
	}

	// A StateChange for an unsubscribed type fires nothing.
	send <- stateChange("acc1", map[string]string{"ContactCard": "x1"})
	select {
	case name := <-signal:
		t.Fatalf("unexpected signal %q for unsubscribed type", name)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if got["Email"] != 1 || got["CalendarEvent"] != 1 {
		t.Fatalf("handler counts = %v, want Email:1 CalendarEvent:1", got)
	}
}

func TestConsumerHandlesLargeStateChange(t *testing.T) {
	// A StateChange touching many accounts exceeds the library's 32 KiB default
	// read limit; the consumer must lift the limit so a legitimate large change
	// does not fatally drop the loop.
	enabled := make(chan pushEnableSeen, 1)
	send := make(chan gojmap.StateChange)
	srv := newPushServer(t, "", enabled, send)

	signal := make(chan string, 1)
	c := New(wsURL(srv.URL), "tok", nil)
	c.Subscribe("Email", func() { signal <- "Email" })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()
	<-enabled

	// Build a StateChange well over 32 KiB by spreading the Email change across
	// many accounts.
	big := gojmap.StateChange{Type: "StateChange", Changed: map[gojmap.ID]gojmap.TypeState{}}
	for i := 0; i < 4000; i++ {
		big.Changed[gojmap.ID("account-padding-identifier-"+strconv.Itoa(i))] = gojmap.TypeState{"Email": "s2"}
	}
	if b, _ := json.Marshal(big); len(b) <= 32768 {
		t.Fatalf("test StateChange is %d bytes, want > 32768 to exercise the limit", len(b))
	}
	send <- big

	if name := waitSignal(t, signal); name != "Email" {
		t.Fatalf("signal = %q, want Email", name)
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestConsumerServeReconnects(t *testing.T) {
	// The first connection delivers one Email StateChange then drops; Serve must
	// reconnect and, on reconnect, fire the handler again to force a catch-up
	// re-sync for changes missed while the socket was down.
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"jmap"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		n := atomic.AddInt32(&conns, 1)
		ctx := r.Context()
		if _, _, err := c.Read(ctx); err != nil { // WebSocketPushEnable
			return
		}
		if n == 1 {
			b, _ := json.Marshal(stateChange("acc1", map[string]string{"Email": "s2"}))
			if err := c.Write(ctx, websocket.MessageText, b); err != nil {
				return
			}
			c.Close(websocket.StatusNormalClosure, "drop") // force a reconnect
			return
		}
		<-ctx.Done() // keep later connections open
	}))
	t.Cleanup(srv.Close)

	signal := make(chan struct{}, 16)
	c := New(wsURL(srv.URL), "tok", nil)
	c.Subscribe("Email", func() { signal <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- c.Serve(ctx, Backoff{Min: 10 * time.Millisecond, Max: 20 * time.Millisecond}) }()

	// First signal: the StateChange on connection #1.
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal from the first connection's StateChange")
	}
	// Second signal: the fireAll forced re-sync after the drop.
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not fire handlers on reconnect")
	}

	// The reconnect dial follows the re-sync; wait for the server to see it.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&conns) < 2 {
		select {
		case <-deadline:
			t.Fatalf("server saw %d connections, want >= 2 (reconnect)", atomic.LoadInt32(&conns))
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestConsumerRejectsBadSubprotocol(t *testing.T) {
	// A server that does not negotiate "jmap" must cause Run to fail rather than
	// silently proceeding on a plain WebSocket.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil) // no subprotocols -> negotiates ""
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := New(wsURL(srv.URL), "tok", nil)
	c.Subscribe("Email", func() {})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Run(ctx); err == nil {
		t.Fatal("Run returned nil for a server that did not negotiate the jmap subprotocol")
	}
}

func waitSignal(t *testing.T, signal <-chan string) string {
	t.Helper()
	select {
	case name := <-signal:
		return name
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler signal")
		return ""
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

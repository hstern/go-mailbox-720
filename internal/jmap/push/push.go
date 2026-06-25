// Package push consumes JMAP server StateChange events over a WebSocket,
// implementing the client side of RFC 8887 ("A JMAP Subprotocol for WebSocket").
//
// JMAP normally delivers change notifications by EventSource (SSE) long-poll or
// an out-of-band PushSubscription. RFC 8887 carries the same StateChange objects
// over a WebSocket: the client opens the socket advertised by the session's
// urn:ietf:params:jmap:websocket capability, negotiating the "jmap" subprotocol,
// sends a WebSocketPushEnable, and then receives StateChange objects as the
// account changes — no polling.
//
// A Consumer is the integration primitive for the change-notification delivery
// loops (the Watcher capability in internal/{mail,calendar,contacts}): it turns
// server push into coalesced per-data-type signals the loops re-sync on. One
// socket carries push for every data type (Email, CalendarEvent, ContactCard,
// …), so a single Consumer can serve the mail, calendar, and contacts watchers
// of one account.
//
// This consumer enables push only; it issues no JMAP API calls over the socket
// (the adapters keep making those over HTTP), so server Response and RequestError
// frames are ignored.
package push

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"github.com/coder/websocket"
)

// Subprotocol is the WebSocket subprotocol token RFC 8887 defines for JMAP. The
// server MUST negotiate it; the consumer refuses a socket that does not.
const Subprotocol = "jmap"

// maxFrameBytes caps a single inbound WebSocket message. StateChange frames are
// small (a per-account map of type→state strings) but unbounded in account
// count, so the cap is generous; it exists only to bound a misbehaving server,
// replacing the library's 32 KiB default which a legitimate large change could
// exceed.
const maxFrameBytes = 1 << 20 // 1 MiB

// Options configures a Consumer.
type Options struct {
	// HTTPClient performs the WebSocket handshake. When nil, http.DefaultClient
	// is used. Authentication travels in the Authorization header (see New), so
	// a custom client is needed only for transport concerns (proxies, test
	// servers), not for auth.
	HTTPClient *http.Client
}

// Consumer maintains one JMAP-over-WebSocket connection for one authenticated
// account and dispatches server StateChange events to per-data-type handlers.
// Subscribe before Run; Run blocks until the context is cancelled.
type Consumer struct {
	wsURL    string
	token    string
	client   *http.Client
	handlers map[string][]func()
}

// New creates a Consumer for the WebSocket endpoint at wsURL (the URL from the
// session's urn:ietf:params:jmap:websocket capability — see WebSocketURL),
// authenticating with the bearer access token.
//
// The caller must have verified WebSocketCapability.SupportsPush before calling
// New: against a server that advertises the socket for request/response only
// (SupportsPush false), Run connects and sends WebSocketPushEnable but no
// StateChange ever arrives, so handlers silently never fire.
func New(wsURL, token string, o *Options) *Consumer {
	if o == nil {
		o = &Options{}
	}
	return &Consumer{
		wsURL:    wsURL,
		token:    token,
		client:   o.HTTPClient,
		handlers: map[string][]func(){},
	}
}

// Subscribe registers fn to run each time the server pushes a StateChange that
// touches dataType (e.g. "Email", "CalendarEvent", "ContactCard"). fn is a
// coalesced signal — it reports that something of that type changed, not what —
// so the caller re-syncs (e.g. via a DeltaReader) to discover specifics. Call
// Subscribe before Run; it is not safe to call concurrently with Run.
func (c *Consumer) Subscribe(dataType string, fn func()) {
	c.handlers[dataType] = append(c.handlers[dataType], fn)
}

// pushEnable is the RFC 8887 WebSocketPushEnable request: it asks the server to
// start sending StateChange objects for the listed data types over the socket.
type pushEnable struct {
	Type      string   `json:"@type"`
	DataTypes []string `json:"dataTypes,omitempty"`
}

// Run dials the WebSocket, enables push for the subscribed data types, and
// dispatches StateChange events until ctx is cancelled or a fatal error occurs.
// A cancelled context is a clean shutdown and returns nil; any other failure
// (handshake, subprotocol mismatch, read or decode) is returned wrapped.
//
// Handlers run synchronously on Run's goroutine, in registration order. A slow
// handler therefore backpressures the read loop; handlers should hand off rather
// than block (the delivery loops signal a buffered channel).
func (c *Consumer) Run(ctx context.Context) error {
	conn, resp, err := websocket.Dial(ctx, c.wsURL, &websocket.DialOptions{
		HTTPClient:   c.client,
		Subprotocols: []string{Subprotocol},
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		return fmt.Errorf("jmap/push: dial: %w", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	defer conn.CloseNow()

	if got := conn.Subprotocol(); got != Subprotocol {
		// A server that ignored the subprotocol requirement is misbehaving; the
		// deferred CloseNow tears the socket down without waiting on a close
		// handshake that such a server may never complete.
		return fmt.Errorf("jmap/push: server negotiated subprotocol %q, want %q", got, Subprotocol)
	}

	// StateChange frames are server-pushed and trusted, and a single frame can
	// touch many accounts/types, so the library's small default read limit
	// (32 KiB) would fatally drop the loop on a large change. Lift it to a
	// generous cap that still bounds a misbehaving server.
	conn.SetReadLimit(maxFrameBytes)

	if err := c.enable(ctx, conn); err != nil {
		return err
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// A cancelled context is the clean-shutdown signal: the library has
			// already torn the connection down to unblock Read, so the deferred
			// CloseNow is the only cleanup needed.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("jmap/push: read: %w", err)
		}
		if err := c.dispatch(data); err != nil {
			return err
		}
	}
}

// enable sends WebSocketPushEnable for the union of subscribed data types. The
// list is sorted so the frame is deterministic (testable, cache-friendly).
func (c *Consumer) enable(ctx context.Context, conn *websocket.Conn) error {
	types := make([]string, 0, len(c.handlers))
	for t := range c.handlers {
		types = append(types, t)
	}
	sort.Strings(types)
	b, err := json.Marshal(pushEnable{Type: "WebSocketPushEnable", DataTypes: types})
	if err != nil {
		return fmt.Errorf("jmap/push: marshal WebSocketPushEnable: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("jmap/push: write WebSocketPushEnable: %w", err)
	}
	return nil
}

// dispatch decodes one server message and, when it is a StateChange, invokes the
// handlers for every changed data type. Non-StateChange messages (Response,
// RequestError) are ignored. A handler fires at most once per message even if
// several accounts report the same type, matching the coalesced-signal contract.
func (c *Consumer) dispatch(data []byte) error {
	var env struct {
		Type string `json:"@type"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("jmap/push: decode message: %w", err)
	}
	if env.Type != "StateChange" {
		return nil
	}
	var sc gojmap.StateChange
	if err := json.Unmarshal(data, &sc); err != nil {
		return fmt.Errorf("jmap/push: decode StateChange: %w", err)
	}
	fired := map[string]bool{}
	for _, ts := range sc.Changed {
		for dataType := range ts {
			if fired[dataType] {
				continue
			}
			fired[dataType] = true
			for _, fn := range c.handlers[dataType] {
				fn()
			}
		}
	}
	return nil
}

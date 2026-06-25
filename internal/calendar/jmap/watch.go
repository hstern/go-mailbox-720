package jmap

import (
	"context"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/jmap/push"
)

// dataTypeCalendarEvent is the JMAP data type whose StateChange signals a
// calendar change. The JMAP calendars specification names the event object
// "CalendarEvent"; a change to any event in the account produces a StateChange
// for this type.
const dataTypeCalendarEvent = "CalendarEvent"

var _ calendar.Watcher = (*Client)(nil)

// Watch implements calendar.Watcher over RFC 8887 JMAP push. It opens a
// WebSocket to the server's urn:ietf:params:jmap:websocket endpoint and invokes
// onChange each time the server reports a CalendarEvent StateChange for the
// account, until ctx is cancelled.
//
// calendarID is advisory: JMAP StateChange is account-wide (it reports that the
// CalendarEvent type changed, not which calendar), so any change signals and the
// caller's delta re-sync scopes back to the calendar it cares about — matching
// the coalesced-signal contract calendar.Watcher documents.
//
// Watch owns its own WebSocket and reconnects with backoff if the socket drops,
// firing onChange once per reconnect so changes missed while disconnected are
// caught by the next re-sync. It returns an error when the server advertises no
// WebSocket push support, or when the session authenticated with HTTP Basic auth
// (the push consumer is bearer-only); transient failures are retried until ctx
// is cancelled, at which point it returns nil.
func (cl *Client) Watch(ctx context.Context, calendarID string, onChange func()) error {
	if cl.token == "" {
		return fmt.Errorf("jmap: WebSocket watch requires bearer auth; this session uses HTTP Basic")
	}
	capab, ok := push.WebSocketURL(cl.c.Session)
	if !ok || !capab.SupportsPush {
		return fmt.Errorf("jmap: server does not advertise WebSocket push (%s); cannot watch", push.CapabilityURI)
	}
	c := push.New(capab.URL, cl.token, nil)
	c.Subscribe(dataTypeCalendarEvent, onChange)
	return c.Serve(ctx, push.Backoff{})
}

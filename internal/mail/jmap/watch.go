package jmap

import (
	"context"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/jmap/push"
	portmail "github.com/hstern/go-mailbox-720/internal/mail"
)

// dataTypeEmail is the JMAP data type whose StateChange signals a mailbox
// change. RFC 8621 names the mail object "Email"; a change to any Email in the
// account produces a StateChange for this type.
const dataTypeEmail = "Email"

var _ portmail.Watcher = (*Client)(nil)

// Watch implements mail.Watcher over RFC 8887 JMAP push. It opens a WebSocket to
// the server's urn:ietf:params:jmap:websocket endpoint and invokes onChange each
// time the server reports an Email StateChange for the account, until ctx is
// cancelled. It is the JMAP analogue of the IMAP IDLE watch.
//
// folderID is advisory: JMAP StateChange is account-wide (it reports that the
// Email type changed, not which mailbox), so any change signals and the caller's
// delta re-sync scopes back to the folder it cares about — matching the
// coalesced-signal contract mail.Watcher documents.
//
// Watch owns its own WebSocket and reconnects with backoff if the socket drops,
// firing onChange once per reconnect so changes missed while disconnected are
// caught by the next re-sync. It returns an error only when the server advertises
// no WebSocket push support; transient failures are retried until ctx is
// cancelled, at which point it returns nil.
func (cl *Client) Watch(ctx context.Context, folderID string, onChange func()) error {
	capab, ok := push.WebSocketURL(cl.c.Session)
	if !ok || !capab.SupportsPush {
		return fmt.Errorf("jmap: server does not advertise WebSocket push (%s); cannot watch", push.CapabilityURI)
	}
	c := push.New(capab.URL, cl.token, nil)
	c.Subscribe(dataTypeEmail, onChange)
	return c.Serve(ctx, push.Backoff{})
}

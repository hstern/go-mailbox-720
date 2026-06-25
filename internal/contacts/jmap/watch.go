package jmap

import (
	"context"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/jmap/push"
)

// dataTypeContactCard is the JMAP data type whose StateChange signals a contacts
// change. RFC 9610 names the contact object "ContactCard"; a change to any card
// in the account produces a StateChange for this type.
const dataTypeContactCard = "ContactCard"

var _ contacts.Watcher = (*Client)(nil)

// Watch implements contacts.Watcher over RFC 8887 JMAP push. It opens a
// WebSocket to the server's urn:ietf:params:jmap:websocket endpoint and invokes
// onChange each time the server reports a ContactCard StateChange for the
// account, until ctx is cancelled.
//
// addressBookID is advisory: JMAP StateChange is account-wide (it reports that
// the ContactCard type changed, not which address book), so any change signals
// and the caller's delta re-sync scopes back to the address book it cares
// about — matching the coalesced-signal contract contacts.Watcher documents.
//
// Watch owns its own WebSocket and reconnects with backoff if the socket drops,
// firing onChange once per reconnect so changes missed while disconnected are
// caught by the next re-sync. It returns an error when the server advertises no
// WebSocket push support; transient failures are retried until ctx is cancelled,
// at which point it returns nil.
func (cl *Client) Watch(ctx context.Context, addressBookID string, onChange func()) error {
	capab, ok := push.WebSocketURL(cl.c.Session)
	if !ok || !capab.SupportsPush {
		return fmt.Errorf("jmap: server does not advertise WebSocket push (%s); cannot watch", push.CapabilityURI)
	}
	c := push.New(capab.URL, cl.token, nil)
	c.Subscribe(dataTypeContactCard, onChange)
	return c.Serve(ctx, push.Backoff{})
}

package imap

import (
	"context"
	"fmt"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

var _ mail.RawReader = (*Client)(nil)

// RawMessage returns the full, unparsed RFC822 bytes of the message with the
// given opaque id, the backing for the iTIP/iMIP scheduling trigger which must
// parse the whole MIME message to find its text/calendar part. Reading does not
// mark the message read.
func (cl *Client) RawMessage(_ context.Context, id string) ([]byte, error) {
	mailbox, uidValidity, uid, err := decodeMessageID(id)
	if err != nil {
		return nil, fmt.Errorf("imap: raw message: %w", err)
	}
	sel, err := cl.c.Select(mailbox, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: raw message: select %q: %w", mailbox, err)
	}
	if sel.UIDValidity != uidValidity {
		return nil, fmt.Errorf("imap: raw message: message id is stale: folder UIDVALIDITY changed")
	}

	bufs, err := cl.c.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{
		// A bare BodySection with no Part/Specifier is the whole message (BODY[] /
		// RFC822) — the full headers and body, not just the text part. Peek so the
		// fetch does not set \Seen: reading a message must not mark it read.
		BodySection: []*goimap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: raw message: fetch: %w", err)
	}
	if len(bufs) == 0 {
		return nil, fmt.Errorf("imap: raw message: message %s not found", id)
	}
	b := bufs[0]
	if len(b.BodySection) == 0 {
		return nil, fmt.Errorf("imap: raw message: message %s returned no body", id)
	}
	return b.BodySection[0].Bytes, nil
}

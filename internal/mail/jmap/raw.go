package jmap

import (
	"context"
	"fmt"
	"io"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

var _ port.RawReader = (*Client)(nil)

// RawMessage returns the full, unparsed RFC822 bytes of the message with the
// given opaque id, the backing for the iTIP/iMIP scheduling trigger which must
// parse the whole MIME message to find its text/calendar part. In JMAP the raw
// message is the email's blob: Email/get yields the message's blobId, then the
// blob download endpoint returns the bytes. Downloading the blob does not change
// the message's keywords, so reading does not mark it read.
func (cl *Client) RawMessage(ctx context.Context, id string) ([]byte, error) {
	emailID, err := decodeMessageID(id)
	if err != nil {
		return nil, fmt.Errorf("jmap: raw message: %w", err)
	}
	blobID, err := cl.blobID(ctx, emailID)
	if err != nil {
		return nil, err
	}
	rc, err := cl.c.DownloadWithContext(ctx, cl.accountID, blobID)
	if err != nil {
		return nil, fmt.Errorf("jmap: raw message: download: %w", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("jmap: raw message: read blob: %w", err)
	}
	return raw, nil
}

// blobID resolves an email's blobId (the handle for its raw RFC822 bytes) via a
// minimal Email/get requesting just that property.
func (cl *Client) blobID(ctx context.Context, emailID gojmap.ID) (gojmap.ID, error) {
	args, err := cl.do(ctx, &email.Get{
		Account:    cl.accountID,
		IDs:        []gojmap.ID{emailID},
		Properties: []string{"id", "blobId"},
	})
	if err != nil {
		return "", err
	}
	resp, ok := args.(*email.GetResponse)
	if !ok {
		return "", fmt.Errorf("jmap: raw message: unexpected response to Email/get: %T", args)
	}
	if len(resp.List) == 0 {
		return "", fmt.Errorf("jmap: raw message: message %s not found", emailID)
	}
	blob := resp.List[0].BlobID
	if blob == "" {
		return "", fmt.Errorf("jmap: raw message: message %s has no blob", emailID)
	}
	return blob, nil
}

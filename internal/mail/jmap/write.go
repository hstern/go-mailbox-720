package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

// Client implements mail.Writer in addition to mail.Backend, so a consumer can
// type-assert the read backend to mail.Writer to reach the message write paths.
var _ port.Writer = (*Client)(nil)

// SetRead sets or clears the message's read state, the backing for Graph's PATCH
// of isRead. In JMAP read state is the $seen keyword, patched with an Email/set
// update whose JSON-pointer path addresses that one keyword — read=true sets
// "keywords/$seen" to true, read=false removes it by setting it to null. This is
// a near pass-through: Graph's isRead is exactly RFC 8621's $seen.
func (cl *Client) SetRead(ctx context.Context, id string, read bool) error {
	emailID, err := decodeMessageID(id)
	if err != nil {
		return err
	}
	var value any // null removes the keyword
	if read {
		value = true
	}
	return cl.set(ctx, &email.Set{
		Account: cl.accountID,
		Update: map[gojmap.ID]gojmap.Patch{
			emailID: {"keywords/" + keywordSeen: value},
		},
	}, emailID, setUpdate)
}

// DeleteMessage removes the message, the backing for Graph's DELETE, via an
// Email/set destroy. JMAP destroy removes the email from every mailbox it is in
// (the account-level delete), matching Graph's single DELETE semantics without
// IMAP's two-step \Deleted+EXPUNGE.
func (cl *Client) DeleteMessage(ctx context.Context, id string) error {
	emailID, err := decodeMessageID(id)
	if err != nil {
		return err
	}
	return cl.set(ctx, &email.Set{
		Account: cl.accountID,
		Destroy: []gojmap.ID{emailID},
	}, emailID, setDestroy)
}

// setKind selects which not-* map of an Email/set response to inspect for a
// per-record error.
type setKind int

const (
	setUpdate setKind = iota
	setDestroy
)

// set runs an Email/set and reports a per-record SetError (in NotUpdated or
// NotDestroyed for the target id) as an error, so a refused write surfaces
// rather than silently succeeding.
func (cl *Client) set(ctx context.Context, m *email.Set, id gojmap.ID, kind setKind) error {
	args, err := cl.do(ctx, m)
	if err != nil {
		return err
	}
	resp, ok := args.(*email.SetResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response to Email/set: %T", args)
	}
	var notDone map[gojmap.ID]*gojmap.SetError
	switch kind {
	case setUpdate:
		notDone = resp.NotUpdated
	case setDestroy:
		notDone = resp.NotDestroyed
	}
	if se, ok := notDone[id]; ok {
		return fmt.Errorf("jmap: set: %s", setErrorString(se))
	}
	return nil
}

func setErrorString(se *gojmap.SetError) string {
	if se == nil {
		return "unknown set error"
	}
	if se.Description != nil {
		return se.Type + ": " + *se.Description
	}
	return se.Type
}

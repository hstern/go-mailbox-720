package jmap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

var _ port.DeltaReader = (*Client)(nil)

// The delta sync-token is, near-verbatim, the JMAP Email state string — the
// cursor RFC 8621's Email/changes consumes. It is base64url wrapped only so the
// token is opaque and URL-safe in a Graph @odata.deltaLink, mirroring id.go.
// Unlike the IMAP adapter there is no UIDVALIDITY to carry: JMAP state is a
// single account-wide string the server defines, so the token is just that
// string. A token the server later rejects as too old yields a JMAP
// "cannotCalculateChanges" error, which Delta turns into a fresh full resync.

func encodeDeltaToken(state string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(state))
}

func decodeDeltaToken(token string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: %v", port.ErrInvalidDeltaToken, err)
	}
	return string(b), nil
}

// Delta reports the messages in folderID changed since token, the backing for
// Graph's GET /me/messages/delta. An empty folderID selects the inbox.
//
// JMAP's Email/changes is account-wide (it has no per-folder variant), so Delta
// runs it against the whole account and then narrows created/updated to the
// folder by checking each changed email's mailboxIds. An empty (initial) token
// returns the folder's current messages plus a fresh token at the current Email
// state. On a subsequent call the token's state is fed to Email/changes; if the
// server cannot calculate changes from it (the state is too old), Delta falls
// back to a full resync rather than failing.
//
// changed is ordered newest-first, consistent with ListMessages; removed holds
// the opaque IDs of destroyed messages, for Graph @removed tombstones.
func (cl *Client) Delta(ctx context.Context, folderID, token string) (changed []port.Message, removed []string, next string, err error) {
	mailboxID, err := cl.resolveFolderID(ctx, folderID)
	if err != nil {
		return nil, nil, "", err
	}

	if token == "" {
		return cl.deltaInitial(ctx, mailboxID)
	}
	sinceState, err := decodeDeltaToken(token)
	if err != nil {
		return nil, nil, "", fmt.Errorf("jmap: delta: %w", err)
	}

	chResp, err := cl.emailChanges(ctx, sinceState)
	if err != nil {
		// A server that cannot calculate changes from this state (too old, or it
		// never could) reports cannotCalculateChanges; resync from scratch.
		if isCannotCalculateChanges(err) {
			return cl.deltaInitial(ctx, mailboxID)
		}
		return nil, nil, "", err
	}

	// Created + updated emails that currently live in the folder are the changed
	// set; destroyed emails are removed tombstones. An updated email that moved
	// out of the folder simply will not have mailboxID in its mailboxIds, so it
	// drops out of changed — Graph re-syncs the folder it moved to separately.
	changedIDs := append(append([]gojmap.ID{}, chResp.Created...), chResp.Updated...)
	if len(changedIDs) > 0 {
		emails, state, err := cl.fetchEmails(ctx, changedIDs, false)
		if err != nil {
			return nil, nil, "", err
		}
		for _, e := range emails {
			if e.MailboxIDs[mailboxID] {
				m := mapEmail(e, false)
				m.ETag = state
				changed = append(changed, m)
			}
		}
	}
	for _, id := range chResp.Destroyed {
		removed = append(removed, messageID(id))
	}
	return changed, removed, encodeDeltaToken(chResp.NewState), nil
}

// deltaInitial returns the folder's current messages and a token at the current
// Email state (an unbounded Email/query for the mailbox, plus a zero-id
// Email/changes only to read the state). It is the initial-sync and resync path.
func (cl *Client) deltaInitial(ctx context.Context, mailboxID gojmap.ID) ([]port.Message, []string, string, error) {
	state, err := cl.emailState(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	query := &email.Query{
		Account: cl.accountID,
		Filter:  &email.FilterCondition{InMailbox: mailboxID},
		Sort:    []*email.SortComparator{{Property: "receivedAt", IsAscending: false}},
	}
	ids, err := cl.queryEmailIDs(ctx, query)
	if err != nil {
		return nil, nil, "", err
	}
	var msgs []port.Message
	if len(ids) > 0 {
		if msgs, err = cl.getEmails(ctx, ids, false); err != nil {
			return nil, nil, "", err
		}
	}
	return msgs, nil, encodeDeltaToken(state), nil
}

// emailChanges runs Email/changes since the given state.
func (cl *Client) emailChanges(ctx context.Context, sinceState string) (*email.ChangesResponse, error) {
	args, err := cl.do(ctx, &email.Changes{Account: cl.accountID, SinceState: sinceState})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*email.ChangesResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response to Email/changes: %T", args)
	}
	return resp, nil
}

// emailState reads the current account-wide Email state by issuing an Email/get
// for no ids (the cheapest call that returns the state string).
func (cl *Client) emailState(ctx context.Context) (string, error) {
	args, err := cl.do(ctx, &email.Get{Account: cl.accountID, IDs: []gojmap.ID{}})
	if err != nil {
		return "", err
	}
	resp, ok := args.(*email.GetResponse)
	if !ok {
		return "", fmt.Errorf("jmap: unexpected response to Email/get: %T", args)
	}
	return resp.State, nil
}

// isCannotCalculateChanges reports whether err is a JMAP cannotCalculateChanges
// method error, the signal that the token's state is too old to diff from.
func isCannotCalculateChanges(err error) bool {
	var me *gojmap.MethodError
	if errors.As(err, &me) {
		return me.Type == "cannotCalculateChanges"
	}
	return false
}

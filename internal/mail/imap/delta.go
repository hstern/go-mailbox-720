package imap

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

var _ mail.DeltaReader = (*Client)(nil)

// deltaToken encodes the sync high-water mark for a folder: its UIDVALIDITY and
// the MODSEQ (CONDSTORE mod-sequence) already reported. Mirroring id.go, the
// tuple is joined with a NUL separator and base64url-encoded into one opaque,
// round-trippable token. UIDVALIDITY is carried so a recreated folder (validity
// changed) can be detected and resynced rather than silently mis-reported.
func encodeDeltaToken(uidValidity uint32, modSeq uint64) string {
	raw := strconv.FormatUint(uint64(uidValidity), 10) + "\x00" + strconv.FormatUint(modSeq, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeDeltaToken(token string) (uidValidity uint32, modSeq uint64, err error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", mail.ErrInvalidDeltaToken, err)
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: bad structure", mail.ErrInvalidDeltaToken)
	}
	uv, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: uidvalidity: %v", mail.ErrInvalidDeltaToken, err)
	}
	ms, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: modseq: %v", mail.ErrInvalidDeltaToken, err)
	}
	return uint32(uv), ms, nil
}

// Delta reports the messages in folderID changed since token, the backing for
// Graph's GET /me/messages/delta. An empty folderID selects the inbox.
//
// Delta requires the CONDSTORE extension (RFC 7162): it tracks a folder's MODSEQ
// (mod-sequence) and asks the server for every message whose MODSEQ advanced
// since the token via FETCH CHANGEDSINCE. Because a new arrival and a
// flag/read-state change both bump a message's MODSEQ, one CHANGEDSINCE pass
// reports both — unlike a UID high-water mark, which sees only arrivals. If the
// server does not advertise CONDSTORE, Delta returns mail.ErrDeltaUnsupported
// rather than silently degrading.
//
// An empty token means initial sync: every current message is returned with a
// fresh token capturing the folder's UIDVALIDITY and current high MODSEQ. On a
// subsequent call the token is decoded; if the folder's UIDVALIDITY no longer
// matches (the folder was deleted and recreated) the MODSEQ is meaningless, so a
// full resync is performed exactly as for an empty token.
//
// Ordering is newest-first, consistent with ListMessages.
//
// LIMITATION: Delta reports created and changed messages, not deletions. IMAP
// reports expunges incrementally only via QRESYNC's VANISHED response, which the
// client library does not support; emitting deletion tombstones is future work
// (MB720-8).
func (cl *Client) Delta(_ context.Context, folderID, token string) ([]mail.Message, string, error) {
	mailbox := "INBOX"
	if folderID != "" {
		var err error
		if mailbox, err = decodeFolderID(folderID); err != nil {
			return nil, "", err
		}
	}

	// An empty token is decoded as a zero MODSEQ; a non-empty token is decoded up
	// front so a malformed one fails locally before any network round-trip.
	var (
		tokUIDValidity uint32
		tokModSeq      uint64
		initial        = token == ""
	)
	if !initial {
		var err error
		if tokUIDValidity, tokModSeq, err = decodeDeltaToken(token); err != nil {
			return nil, "", fmt.Errorf("imap: delta: %w", err)
		}
	}

	// Require CONDSTORE: incremental sync is built on MODSEQ. Refuse rather than
	// silently fall back to additive-only sync. Caps() has CONDSTORE when the
	// server advertises CONDSTORE or QRESYNC (which implies it).
	if !cl.c.Caps().Has(goimap.CapCondStore) {
		return nil, "", mail.ErrDeltaUnsupported
	}

	// SELECT (CONDSTORE) enables MODSEQ tracking for this session and returns the
	// folder's current high MODSEQ.
	sel, err := cl.c.Select(mailbox, &goimap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		return nil, "", fmt.Errorf("imap: delta: select %q: %w", mailbox, err)
	}

	// Initial sync, or a recreated folder whose UIDVALIDITY no longer matches the
	// token: report everything currently present and mint a token at the current
	// high MODSEQ.
	if initial || tokUIDValidity != sel.UIDValidity {
		return cl.deltaFull(mailbox, sel)
	}

	// Incremental: FETCH every message changed since the token's MODSEQ. New
	// arrivals and flag changes both advance MODSEQ, so CHANGEDSINCE captures both;
	// an unchanged folder returns nothing. The next token is the folder's current
	// high MODSEQ.
	msgs, err := cl.fetchDelta(mailbox, sel.UIDValidity, goimap.UIDSet{{Start: 1, Stop: 0}}, tokModSeq)
	if err != nil {
		return nil, "", err
	}
	return msgs, encodeDeltaToken(sel.UIDValidity, sel.HighestModSeq), nil
}

// deltaFull returns every current message in the selected mailbox plus a token
// encoding its UIDVALIDITY and current high MODSEQ. It backs both the
// initial-sync and UIDVALIDITY-reset paths.
func (cl *Client) deltaFull(mailbox string, sel *goimap.SelectData) ([]mail.Message, string, error) {
	if sel.NumMessages == 0 {
		return nil, encodeDeltaToken(sel.UIDValidity, sel.HighestModSeq), nil
	}
	msgs, err := cl.fetchDelta(mailbox, sel.UIDValidity, goimap.SeqSet{{Start: 1, Stop: sel.NumMessages}}, 0)
	if err != nil {
		return nil, "", err
	}
	return msgs, encodeDeltaToken(sel.UIDValidity, sel.HighestModSeq), nil
}

// fetchDelta FETCHes the envelope-level data for numSet in the already-selected
// mailbox and maps each buffer to a mail.Message (newest-first, like
// ListMessages). When changedSince is non-zero the FETCH carries CHANGEDSINCE, so
// only messages whose MODSEQ exceeds it are returned; zero fetches the whole set.
// The mailbox is assumed already selected with CONDSTORE.
func (cl *Client) fetchDelta(mailbox string, uidValidity uint32, numSet goimap.NumSet, changedSince uint64) ([]mail.Message, error) {
	bufs, err := cl.c.Fetch(numSet, &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
		ModSeq:       true,
		ChangedSince: changedSince,
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: delta: fetch: %w", err)
	}
	msgs := make([]mail.Message, 0, len(bufs))
	for _, b := range bufs {
		msgs = append(msgs, envelopeMessage(mailbox, uidValidity, b))
	}
	slices.Reverse(msgs) // FETCH yields ascending UID; Graph wants newest first
	return msgs, nil
}

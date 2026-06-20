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
// Delta requires the QRESYNC extension (RFC 7162), which implies CONDSTORE. It
// tracks a folder's MODSEQ (mod-sequence) and asks the server, in one UID FETCH
// CHANGEDSINCE … VANISHED, for every message whose MODSEQ advanced since the
// token (changed) and the UIDs expunged since it (removed). Because a new arrival
// and a flag/read-state change both bump MODSEQ, CHANGEDSINCE reports both; the
// VANISHED response reports deletions. If the server does not advertise QRESYNC,
// Delta returns mail.ErrDeltaUnsupported rather than silently degrading.
//
// An empty token means initial sync: every current message is returned with a
// fresh token capturing the folder's UIDVALIDITY and current high MODSEQ, and no
// removals. On a subsequent call the token is decoded; if the folder's
// UIDVALIDITY no longer matches (the folder was deleted and recreated) the MODSEQ
// is meaningless, so a full resync is performed exactly as for an empty token.
//
// changed is ordered newest-first, consistent with ListMessages; removed holds
// the opaque IDs of expunged messages, for Graph @removed tombstones.
func (cl *Client) Delta(_ context.Context, folderID, token string) (changed []mail.Message, removed []string, next string, err error) {
	mailbox := "INBOX"
	if folderID != "" {
		if mailbox, err = decodeFolderID(folderID); err != nil {
			return nil, nil, "", err
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
		if tokUIDValidity, tokModSeq, err = decodeDeltaToken(token); err != nil {
			return nil, nil, "", fmt.Errorf("imap: delta: %w", err)
		}
	}

	// Require QRESYNC: incremental sync is built on MODSEQ (CONDSTORE) and VANISHED
	// (QRESYNC). Refuse rather than silently degrade.
	if !cl.c.Caps().Has(goimap.CapQResync) {
		return nil, nil, "", mail.ErrDeltaUnsupported
	}

	// SELECT (CONDSTORE) enables MODSEQ tracking for this session and returns the
	// folder's current high MODSEQ.
	sel, err := cl.c.Select(mailbox, &goimap.SelectOptions{CondStore: true}).Wait()
	if err != nil {
		return nil, nil, "", fmt.Errorf("imap: delta: select %q: %w", mailbox, err)
	}

	// Initial sync, or a recreated folder whose UIDVALIDITY no longer matches the
	// token: report everything currently present, no removals, token at the
	// current high MODSEQ.
	if initial || tokUIDValidity != sel.UIDValidity {
		changed, err = cl.fetchDelta(mailbox, sel.UIDValidity, fullSet(sel), 0)
		if err != nil {
			return nil, nil, "", err
		}
		return changed, nil, encodeDeltaToken(sel.UIDValidity, sel.HighestModSeq), nil
	}

	// Incremental: one UID FETCH 1:* (… CHANGEDSINCE <modseq> VANISHED) returns the
	// changed messages and, via VANISHED responses collected into cl.vanished, the
	// UIDs expunged since the token. New arrivals and flag changes both advance
	// MODSEQ, so CHANGEDSINCE captures both; an unchanged folder returns nothing.
	cl.mu.Lock()
	cl.vanished = nil
	cl.mu.Unlock()

	changed, err = cl.fetchDelta(mailbox, sel.UIDValidity, goimap.UIDSet{{Start: 1, Stop: 0}}, tokModSeq)
	if err != nil {
		return nil, nil, "", err
	}

	cl.mu.Lock()
	vanished := cl.vanished
	cl.vanished = nil
	cl.mu.Unlock()
	if uids, ok := vanished.Nums(); ok {
		removed = make([]string, 0, len(uids))
		for _, uid := range uids {
			removed = append(removed, messageID(mailbox, sel.UIDValidity, uint32(uid)))
		}
	}

	return changed, removed, encodeDeltaToken(sel.UIDValidity, sel.HighestModSeq), nil
}

// fullSet is the FETCH set for a full resync: all messages by sequence number, or
// nil when the mailbox is empty (caller fetches nothing).
func fullSet(sel *goimap.SelectData) goimap.NumSet {
	if sel.NumMessages == 0 {
		return goimap.SeqSet(nil)
	}
	return goimap.SeqSet{{Start: 1, Stop: sel.NumMessages}}
}

// fetchDelta FETCHes the envelope-level data for numSet in the already-selected
// mailbox and maps each buffer to a mail.Message (newest-first, like
// ListMessages). When changedSince is non-zero the FETCH carries CHANGEDSINCE (so
// only messages whose MODSEQ exceeds it are returned) plus the VANISHED modifier,
// which makes the server report expunged UIDs as VANISHED responses (collected by
// the dial-time handler into cl.vanished). A zero changedSince fetches the whole
// set with no VANISHED. The mailbox is assumed already selected with CONDSTORE.
func (cl *Client) fetchDelta(mailbox string, uidValidity uint32, numSet goimap.NumSet, changedSince uint64) ([]mail.Message, error) {
	if set, ok := numSet.(goimap.SeqSet); ok && len(set) == 0 {
		return nil, nil // empty mailbox: nothing to fetch
	}
	bufs, err := cl.c.Fetch(numSet, &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
		ModSeq:       true,
		ChangedSince: changedSince,
		Vanished:     changedSince != 0, // VANISHED is only valid with CHANGEDSINCE
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

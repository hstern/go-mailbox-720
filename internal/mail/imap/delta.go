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
// the highest UID already reported. Mirroring id.go, the tuple is joined with a
// NUL separator and base64url-encoded into one opaque, round-trippable token.
// UIDVALIDITY is carried so a recreated folder (validity changed) can be
// detected and resynced rather than silently mis-reported.
func encodeDeltaToken(uidValidity, lastUID uint32) string {
	raw := strconv.FormatUint(uint64(uidValidity), 10) + "\x00" + strconv.FormatUint(uint64(lastUID), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeDeltaToken(token string) (uidValidity, lastUID uint32, err error) {
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
	lu, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: uid: %v", mail.ErrInvalidDeltaToken, err)
	}
	return uint32(uv), uint32(lu), nil
}

// Delta reports the messages in folderID that are new since token, the backing
// for Graph's GET /me/messages/delta. An empty folderID selects the inbox.
//
// An empty token means initial sync: every current message is returned along
// with a fresh token capturing the folder's UIDVALIDITY and highest UID. On a
// subsequent call that token is decoded; if the folder's UIDVALIDITY no longer
// matches (the folder was deleted and recreated) the high-water mark is
// meaningless, so a full resync is performed exactly as for an empty token.
// Otherwise an IMAP UID SEARCH for UID (lastUID+1):* finds the arrivals, which
// are fetched and returned with an advanced token; when nothing new arrived the
// old high-water mark is preserved so the token stays stable.
//
// Ordering is newest-first, consistent with ListMessages.
//
// LIMITATION: this first cut is additive only. It reports newly-arrived
// messages by UID; it does not report deletions or flag/read-state changes,
// which would require IMAP CONDSTORE/QRESYNC (MODSEQ tracking) and is future
// work (MB720-8).
func (cl *Client) Delta(_ context.Context, folderID, token string) ([]mail.Message, string, error) {
	mailbox := "INBOX"
	if folderID != "" {
		var err error
		if mailbox, err = decodeFolderID(folderID); err != nil {
			return nil, "", err
		}
	}

	// An empty token is decoded as a zero high-water mark; a non-empty token is
	// decoded up front so a malformed one fails before any network round-trip.
	var (
		tokUIDValidity, tokLastUID uint32
		initial                    = token == ""
	)
	if !initial {
		var err error
		if tokUIDValidity, tokLastUID, err = decodeDeltaToken(token); err != nil {
			return nil, "", fmt.Errorf("imap: delta: %w", err)
		}
	}

	sel, err := cl.c.Select(mailbox, nil).Wait()
	if err != nil {
		return nil, "", fmt.Errorf("imap: delta: select %q: %w", mailbox, err)
	}

	// Initial sync, or a recreated folder whose UIDVALIDITY no longer matches the
	// token: report everything currently present and mint a fresh token.
	if initial || tokUIDValidity != sel.UIDValidity {
		return cl.deltaFull(mailbox, sel)
	}

	// Incremental: search for the UIDs above the recorded high-water mark. Note
	// the RFC 3501 gotcha that a range like N:* always matches the message with
	// the highest UID even when N exceeds it, so the result must be re-filtered to
	// UIDs strictly greater than lastUID — otherwise an unchanged mailbox would
	// re-report its last message.
	criteria := &goimap.SearchCriteria{UID: []goimap.UIDSet{{{Start: goimap.UID(tokLastUID) + 1, Stop: 0}}}}
	data, err := cl.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, "", fmt.Errorf("imap: delta: search: %w", err)
	}
	var uids []goimap.UID
	for _, u := range data.AllUIDs() {
		if uint32(u) > tokLastUID {
			uids = append(uids, u)
		}
	}
	if len(uids) == 0 {
		// Nothing new: keep the existing high-water mark so the token is stable.
		return nil, encodeDeltaToken(sel.UIDValidity, tokLastUID), nil
	}

	msgs, highUID, err := cl.fetchDelta(mailbox, sel.UIDValidity, goimap.UIDSetNum(uids...))
	if err != nil {
		return nil, "", err
	}
	if highUID < tokLastUID {
		highUID = tokLastUID
	}
	return msgs, encodeDeltaToken(sel.UIDValidity, highUID), nil
}

// deltaFull returns every current message in the selected mailbox plus a token
// encoding its UIDVALIDITY and highest UID (0 for an empty mailbox). It backs
// both the initial-sync and UIDVALIDITY-reset paths.
func (cl *Client) deltaFull(mailbox string, sel *goimap.SelectData) ([]mail.Message, string, error) {
	if sel.NumMessages == 0 {
		return nil, encodeDeltaToken(sel.UIDValidity, 0), nil
	}
	msgs, highUID, err := cl.fetchDelta(mailbox, sel.UIDValidity, goimap.SeqSet{{Start: 1, Stop: sel.NumMessages}})
	if err != nil {
		return nil, "", err
	}
	return msgs, encodeDeltaToken(sel.UIDValidity, highUID), nil
}

// fetchDelta FETCHes the envelope-level data for numSet in the already-selected
// mailbox, maps each buffer to a mail.Message (newest-first, like ListMessages),
// and reports the highest UID seen. The mailbox is assumed already selected.
func (cl *Client) fetchDelta(mailbox string, uidValidity uint32, numSet goimap.NumSet) ([]mail.Message, uint32, error) {
	bufs, err := cl.c.Fetch(numSet, &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
	}).Collect()
	if err != nil {
		return nil, 0, fmt.Errorf("imap: delta: fetch: %w", err)
	}
	var highUID uint32
	msgs := make([]mail.Message, 0, len(bufs))
	for _, b := range bufs {
		if uid := uint32(b.UID); uid > highUID {
			highUID = uid
		}
		msgs = append(msgs, envelopeMessage(mailbox, uidValidity, b))
	}
	slices.Reverse(msgs) // FETCH yields ascending UID; Graph wants newest first
	return msgs, highUID, nil
}

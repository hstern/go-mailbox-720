package imap

import (
	"context"
	"fmt"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// Client implements mail.Writer in addition to mail.Backend, so a consumer can
// type-assert the read backend to mail.Writer to reach the message write paths.
var _ mail.Writer = (*Client)(nil)

// selectForUID decodes an opaque message id, selects its mailbox, and verifies
// the folder's UIDVALIDITY still matches the one baked into the id. A mismatch
// means the folder was recreated and the id now points at a different (or no)
// message, so it is rejected rather than acted on — the same staleness guard
// GetMessage applies. It returns the located UID.
func (cl *Client) selectForUID(id string) (uint32, error) {
	mailbox, uidValidity, uid, err := decodeMessageID(id)
	if err != nil {
		return 0, err
	}
	sel, err := cl.c.Select(mailbox, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("imap: select %q: %w", mailbox, err)
	}
	if sel.UIDValidity != uidValidity {
		return 0, fmt.Errorf("imap: message id is stale: folder UIDVALIDITY changed")
	}
	return uid, nil
}

// SetRead sets or clears the \Seen flag on the message via a UID STORE, the
// backing for Graph's PATCH of isRead. read=true adds \Seen (+FLAGS), read=false
// removes it (-FLAGS). The STORE is silent — the server's untagged FETCH reply is
// not needed — but go-imap still returns a FetchCommand that must be drained, so
// the command is closed via Wait/Close.
func (cl *Client) SetRead(_ context.Context, id string, read bool) error {
	uid, err := cl.selectForUID(id)
	if err != nil {
		return err
	}
	op := goimap.StoreFlagsDel
	if read {
		op = goimap.StoreFlagsAdd
	}
	store := cl.c.Store(goimap.UIDSetNum(goimap.UID(uid)), &goimap.StoreFlags{
		Op:     op,
		Silent: true,
		Flags:  []goimap.Flag{goimap.FlagSeen},
	}, nil)
	if err := store.Close(); err != nil {
		return fmt.Errorf("imap: store \\Seen: %w", err)
	}
	return nil
}

// DeleteMessage removes the message, the backing for Graph's DELETE. IMAP has no
// single delete: it marks the message \Deleted with a UID STORE, then expunges
// it. UID EXPUNGE (UIDPLUS / IMAP4rev2) is preferred so only this UID is removed;
// when the server lacks UIDPLUS it falls back to a plain EXPUNGE, which removes
// every \Deleted message in the mailbox — acceptable here because we only just
// added \Deleted to a single message.
func (cl *Client) DeleteMessage(_ context.Context, id string) error {
	uid, err := cl.selectForUID(id)
	if err != nil {
		return err
	}
	uidSet := goimap.UIDSetNum(goimap.UID(uid))
	store := cl.c.Store(uidSet, &goimap.StoreFlags{
		Op:     goimap.StoreFlagsAdd,
		Silent: true,
		Flags:  []goimap.Flag{goimap.FlagDeleted},
	}, nil)
	if err := store.Close(); err != nil {
		return fmt.Errorf("imap: store \\Deleted: %w", err)
	}

	// Prefer UID EXPUNGE so only this message is removed; fall back to EXPUNGE
	// when the server advertises neither UIDPLUS nor IMAP4rev2.
	if cl.c.Caps().Has(goimap.CapUIDPlus) || cl.c.Caps().Has(goimap.CapIMAP4rev2) {
		if _, err := cl.c.UIDExpunge(uidSet).Collect(); err != nil {
			return fmt.Errorf("imap: uid expunge: %w", err)
		}
		return nil
	}
	if _, err := cl.c.Expunge().Collect(); err != nil {
		return fmt.Errorf("imap: expunge: %w", err)
	}
	return nil
}

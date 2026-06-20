// Package mail defines the mailbox backing-store port: a backend-neutral,
// Graph/JMAP-shaped view of mail that the server maps Microsoft Graph requests
// onto. The IMAP adapter (internal/mail/imap) is the first implementation; a
// JMAP adapter can drop in behind the same interface later (MB720-14).
//
// The port deliberately holds no mail of its own — each method round-trips to
// the operator's existing IMAP (later JMAP) server. Message and folder IDs are
// opaque and stable, derived from backend identifiers so a Graph client can
// round-trip them.
package mail

import (
	"context"
	"errors"
	"time"

	"github.com/hstern/go-mailbox-720/internal/odata"
)

// ErrInvalidDeltaToken marks a delta continuation token that cannot be decoded
// (malformed, or from an incompatible encoding). A DeltaReader wraps it so the
// HTTP layer can map an invalid client-supplied token to a resync response
// rather than a server error.
var ErrInvalidDeltaToken = errors.New("mail: invalid delta token")

// Address is a parsed mailbox address (display name + addr-spec).
type Address struct {
	Name  string
	Email string
}

// Body is a message body in a single representation.
type Body struct {
	ContentType string // "text" or "html"
	Content     string
}

// Message is a mail message in Graph/JMAP object shape. List operations populate
// the envelope-level fields cheaply; Body is filled only by GetMessage.
type Message struct {
	ID             string
	FolderID       string
	Subject        string
	From           Address
	To             []Address
	Cc             []Address
	Bcc            []Address
	SentAt         time.Time
	ReceivedAt     time.Time
	Preview        string
	Body           Body
	IsRead         bool
	HasAttachments bool
}

// MailFolder is a mailbox folder (an IMAP mailbox).
type MailFolder struct {
	ID          string
	DisplayName string
	Total       int
	Unread      int
}

// Page bounds a listing. Top is the maximum number of messages to return (0 =
// the backend's default); Skip is the number to skip from the newest.
type Page struct {
	Top  int
	Skip int
}

// Backend is the mailbox backing-store port. Implementations adapt a concrete
// server (IMAP first) to this neutral shape. A Backend is bound to a single
// authenticated mailbox identity.
//
// Out of scope for the first cut (tracked in their own issues): change
// subscriptions / push (MB720-9), delta sync tokens (MB720-8), and message
// submission.
type Backend interface {
	// ListMailFolders returns the mailbox's folders.
	ListMailFolders(ctx context.Context) ([]MailFolder, error)
	// ListMessages returns messages in a folder, newest first, bounded by page.
	// An optional parsed OData $filter narrows the result: a nil filter means
	// "no filter" (return every message in the folder, the default). The filter
	// is a neutral query representation produced and validated by the odata
	// package; the backend translates as much of it as the underlying server can
	// express natively (IMAP SEARCH) and evaluates the remainder client-side, so
	// correctness never depends on full native filter support.
	ListMessages(ctx context.Context, folderID string, page Page, filter *odata.Filter) ([]Message, error)
	// GetMessage returns a single message (including its body) by opaque ID.
	GetMessage(ctx context.Context, id string) (Message, error)
	// Close releases the backend connection.
	Close() error
}

// Writer is the optional message write capability: change a message's read
// state and delete a message. It is kept separate from Backend so that a
// read-only adapter (or the server's read-path fakes) need not implement
// writes, and so that adding writes does not disturb Backend's existing
// implementers. An adapter that supports writes implements Writer in addition
// to Backend; consumers type-assert for it:
//
//	if w, ok := backend.(mail.Writer); ok {
//		err := w.SetRead(ctx, id, true)
//	}
//
// These map onto Microsoft Graph's PATCH /me/messages/{id} (isRead) and
// DELETE /me/messages/{id}. A Writer is bound to the same authenticated mailbox
// identity as its Backend.
type Writer interface {
	// SetRead sets (read=true) or clears (read=false) the message's read state,
	// the backing for Graph's PATCH of isRead. The opaque id locates the message.
	SetRead(ctx context.Context, id string, read bool) error
	// DeleteMessage removes the message with the given opaque id, the backing for
	// Graph's DELETE.
	DeleteMessage(ctx context.Context, id string) error
}

// DeltaReader is the optional incremental-sync capability: report the messages
// that have changed in a folder since a prior point, identified by an opaque
// token. It is kept separate from Backend (like Writer) so that an adapter
// without delta support, and the server's read-path fakes, need not implement
// it, and so adding it does not disturb Backend's existing implementers. An
// adapter that supports delta implements DeltaReader in addition to Backend;
// consumers type-assert for it:
//
//	if d, ok := backend.(mail.DeltaReader); ok {
//		msgs, next, err := d.Delta(ctx, folderID, token)
//	}
//
// This is the backing for Microsoft Graph's GET /me/messages/delta. A
// DeltaReader is bound to the same authenticated mailbox identity as its
// Backend.
type DeltaReader interface {
	// Delta returns the messages in folderID that are new since the opaque
	// token. An empty token means initial sync: return the current messages
	// and a fresh token capturing the sync state. The returned next token is
	// fed back on the following call. This first cut is ADDITIVE — it reports
	// newly-arrived messages (by UID); reporting deletions/flag changes needs
	// IMAP CONDSTORE/QRESYNC and is future work (note this in the doc).
	Delta(ctx context.Context, folderID string, token string) (msgs []Message, next string, err error)
}

// RawReader is the optional raw-message capability: fetch the full, unparsed
// RFC822 bytes of a message by its opaque id. It is kept separate from Backend
// (like Writer, DeltaReader, and Watcher) so that an adapter without raw access,
// and the server's read-path fakes, need not implement it, and so adding it does
// not disturb Backend's existing implementers. An adapter that supports raw reads
// implements RawReader in addition to Backend; consumers type-assert for it:
//
//	if r, ok := backend.(mail.RawReader); ok {
//		raw, err := r.RawMessage(ctx, id)
//	}
//
// It is the primitive the iTIP/iMIP scheduling trigger (MB720-10) builds on:
// finding a message's text/calendar part requires the whole MIME message, which
// Backend.GetMessage's parsed body does not expose. A RawReader is bound to the
// same authenticated mailbox identity as its Backend.
type RawReader interface {
	// RawMessage returns the full RFC822 bytes of the message with the given
	// opaque id, without marking it read. The bytes are the wire form a MIME
	// parser (e.g. the scheduling engine) can consume directly.
	RawMessage(ctx context.Context, id string) ([]byte, error)
}

// Watcher is the optional change-watch capability: block on a folder and signal
// whenever it changes (a message arrives or is removed). It is the primitive the
// change-notification delivery loop (subscriptions, MB720-9) and the scheduling
// trigger (MB720-10) build on. Like Writer and DeltaReader it is kept separate
// from Backend so that an adapter without watch support, and the server's
// read-path fakes, need not implement it, and so adding it does not disturb
// Backend's existing implementers. An adapter that supports watching implements
// Watcher in addition to Backend; consumers type-assert for it:
//
//	if wch, ok := backend.(mail.Watcher); ok {
//		err := wch.Watch(ctx, folderID, func() { resync() })
//	}
//
// A Watcher is bound to the same authenticated mailbox identity as its Backend.
//
// In the IMAP adapter Watch is implemented with IDLE, which monopolizes the
// connection: while a Watch is running no other command can be sent on that
// session. A Watcher should therefore own a dedicated Backend/connection rather
// than sharing one with the read/write paths.
type Watcher interface {
	// Watch blocks until ctx is cancelled (or an error occurs), invoking
	// onChange each time the folder changes (a message arrives or is removed).
	// onChange is a coalesced signal, NOT a description of what changed — the
	// caller re-syncs (e.g. via DeltaReader) to discover specifics. An empty
	// folderID watches the inbox. onChange may fire once more concurrently with,
	// or just after, Watch returns, so a caller must not free state onChange
	// captures until it is sure no such call is in flight.
	Watch(ctx context.Context, folderID string, onChange func()) error
}

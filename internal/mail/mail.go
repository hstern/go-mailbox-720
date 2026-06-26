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

// ErrDeltaUnsupported marks a backend that cannot serve incremental delta sync —
// for IMAP, a server that does not advertise QRESYNC, which delta requires to
// track changes by MODSEQ (CONDSTORE, which QRESYNC implies) and to learn which
// UIDs were expunged (VANISHED). A DeltaReader returns it instead of silently
// degrading; the HTTP layer maps it to 501 so the operator learns the backing
// server is unsuitable for delta.
var ErrDeltaUnsupported = errors.New("mail: backend does not support delta sync (IMAP QRESYNC required)")

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
	// Flagged reports the message is flagged for follow-up — the IMAP \Flagged
	// system flag / JMAP $flagged keyword. It maps onto Graph's flag.flagStatus.
	Flagged bool
	// IsDraft reports the message is a draft — the IMAP \Draft system flag / JMAP
	// $draft keyword. It maps onto Graph's isDraft.
	IsDraft bool
	// Categories are the message's user-assigned labels: IMAP keywords / JMAP
	// keywords that are not system flags (no leading "\" or "$"). They map onto
	// Graph's categories. The system flags with a dedicated Graph field (\Seen,
	// \Flagged, \Draft) are surfaced through IsRead/Flagged/IsDraft instead; other
	// $-namespaced system keywords ($Forwarded, $MDNSent, $Junk, …) have no Graph
	// surface and are dropped. Sorted, for a deterministic order.
	Categories []string
	// ETag is an opaque optimistic-concurrency token for the message, backing
	// Graph's @odata.etag and the If-Match a conditional read-state change
	// (ConditionalWriter) sends back. NOTE: the JMAP backend has no per-message
	// version — this is the ACCOUNT-LEVEL Email state (RFC 8620 §1.6.2), the same
	// value for every message in a response, so it is coarse: any change to any
	// email in the account invalidates it, yielding false 412 conflicts on
	// unrelated concurrent changes. It is best-effort until the per-object JMAP
	// conditional draft (draft-gondwana-jmap-conditional, MB720-39) lands in the
	// library and the server. Empty when the backend exposes no state — notably the
	// IMAP adapter, which has no equivalent and ignores If-Match.
	ETag string
}

// MailFolder is a mailbox folder (an IMAP mailbox).
type MailFolder struct {
	ID          string
	DisplayName string
	Total       int
	Unread      int
	// WellKnownName is the Graph well-known folder name for a special-use folder
	// ("inbox", "sentitems", "drafts", "deleteditems", "junkemail", "archive"),
	// derived from the IMAP RFC 6154 SPECIAL-USE attribute or the JMAP mailbox
	// role. Empty for an ordinary folder. The server uses it to resolve Graph's
	// well-known folder path aliases (GET /me/mailFolders/{inbox|sentitems|…}) to
	// this folder; it is not part of the Graph folder response body, since the
	// spec subset's mailFolder type carries no wellKnownName property.
	WellKnownName string
}

// IsUserKeyword reports whether an IMAP flag / JMAP keyword is a user-defined
// label — a Graph category — rather than a system flag. System flags carry a
// leading "\" (IMAP, e.g. \Seen) or "$" (the IANA IMAP/JMAP keyword namespace,
// e.g. $Forwarded, $Junk); everything else is a user keyword. Both mail adapters
// share this rule so Categories means the same thing regardless of backend.
func IsUserKeyword(s string) bool {
	return s != "" && s[0] != '\\' && s[0] != '$'
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

// ErrPreconditionFailed is returned by a ConditionalWriter when an If-Match
// precondition fails: the mailbox state no longer matches the ETag the caller
// supplied. The HTTP layer maps it to 412 Precondition Failed.
var ErrPreconditionFailed = errors.New("mail: precondition failed (state mismatch)")

// ConditionalWriter is the optional capability to change a message's read state
// only if the mailbox's state still matches a caller-supplied ETag — optimistic
// concurrency, the backing for Microsoft Graph's PATCH /me/messages/{id} (isRead)
// carrying an If-Match header. Like the other capabilities it is type-asserted:
//
//	if cw, ok := backend.(mail.ConditionalWriter); ok {
//		err := cw.SetReadIfMatch(ctx, id, true, ifMatch)
//	}
//
// CAVEAT: on the JMAP backend the precondition is ACCOUNT-LEVEL (Email/set
// ifInState), not per-message (see Message.ETag) — coarse and best-effort, so an
// unrelated concurrent change can spuriously fail it. The IMAP adapter has no
// equivalent and does not implement this capability at all; the server then falls
// back to the unconditional Writer.SetRead, silently ignoring If-Match. A
// ConditionalWriter is bound to the same authenticated mailbox identity as its
// Backend.
type ConditionalWriter interface {
	// SetReadIfMatch sets (read=true) or clears (read=false) the message's read
	// state, but only if the mailbox's current ETag equals ifMatch, returning
	// ErrPreconditionFailed on mismatch. ifMatch is the opaque ETag the caller last
	// observed (Graph's If-Match); an empty ifMatch is an error — callers with no
	// precondition use Writer.SetRead instead.
	SetReadIfMatch(ctx context.Context, id string, read bool, ifMatch string) error
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
	// Delta returns the messages in folderID changed since the opaque token. An
	// empty token means initial sync: return the current messages and a fresh
	// token capturing the sync state. The returned next token is fed back on the
	// following call.
	//
	// changed holds created and modified messages (a flag/read-state change
	// re-reports the message); removed holds the opaque IDs of expunged messages,
	// for Graph @removed tombstones. Implementations that cannot track changes
	// (the IMAP adapter needs QRESYNC) return ErrDeltaUnsupported.
	Delta(ctx context.Context, folderID string, token string) (changed []Message, removed []string, next string, err error)
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

// Quota is a mailbox's storage usage, in bytes. Total is the limit; a Total of 0
// means the mailbox has no storage limit (or the server reports none).
type Quota struct {
	Used  int64
	Total int64
}

// QuotaReader is the optional capability to report mailbox storage usage — the
// IMAP QUOTA extension (RFC 9208, obsoleting RFC 2087) on the IMAP adapter. Like
// the other capabilities it is kept separate from Backend so an adapter without
// quota support need not implement it; consumers type-assert for it:
//
//	if qr, ok := backend.(mail.QuotaReader); ok {
//		q, err := qr.Quota(ctx)
//	}
//
// A QuotaReader is bound to the same authenticated mailbox identity as its Backend.
type QuotaReader interface {
	// Quota returns the mailbox's storage usage. It reports ErrNoQuota when the
	// server exposes no storage quota for the mailbox.
	Quota(ctx context.Context) (Quota, error)
}

// ErrNoQuota is returned by QuotaReader.Quota when the server reports no storage
// quota for the mailbox.
var ErrNoQuota = errors.New("mail: no storage quota")

// MessageRule is a backend-neutral inbox rule (a mail filter): an ordered,
// optionally-disabled rule whose Conditions, when all match an arriving message,
// trigger its Actions. It is the neutral shape the server maps Microsoft Graph's
// messageRule onto, and that a FilterReader/FilterWriter adapter translates to the
// backend filter mechanism — Sieve managed over ManageSieve (RFC 5804) on the IMAP
// tier, or JMAP for Sieve Scripts (RFC 9661) on the JMAP tier (MB720-19).
//
// The model is a deliberate MVP: it carries the common conditions and actions that
// have clean Sieve equivalents. Richer Graph predicates (importance, sensitivity,
// size ranges, the many is* flags) and actions (categories, markImportance,
// permanentDelete) are out of scope until a backend needs them; mapping a Graph
// rule that uses them is lossy — the unsupported members are dropped, not rejected.
type MessageRule struct {
	// ID is an opaque, stable rule identifier the backend assigns (e.g. derived
	// from the rule's position or name in the Sieve script). Empty on a rule being
	// created; the FilterWriter stamps it.
	ID string
	// DisplayName is the human-readable rule name (Graph messageRule.displayName).
	DisplayName string
	// Sequence orders rule evaluation, lower first, mirroring Graph's
	// messageRule.sequence and the top-to-bottom order of a Sieve script.
	Sequence int
	// Enabled reports whether the rule is applied (Graph messageRule.isEnabled).
	Enabled    bool
	Conditions RuleConditions
	Actions    RuleActions
}

// RuleConditions is the conjunction of predicates a message must satisfy for a
// rule to fire: every set condition must match (an empty RuleConditions matches
// every message). It models the subset of Graph messageRulePredicates with clean
// Sieve equivalents. Each *Contains slice matches when any of its substrings is
// present (the values are OR-ed within a field, AND-ed across fields).
type RuleConditions struct {
	// SubjectContains matches a substring of the Subject (Sieve: header :contains
	// "subject").
	SubjectContains []string
	// BodyContains matches a substring of the body (Sieve: body :contains).
	BodyContains []string
	// SenderContains matches a substring of the From header (Sieve: header
	// :contains "from").
	SenderContains []string
	// FromAddresses matches when the message's From address equals one of these
	// (Sieve: address :is "from").
	FromAddresses []Address
	// SentToAddresses matches when a To address equals one of these (Sieve:
	// address :is "to").
	SentToAddresses []Address
}

// RuleActions is the set of actions taken when a rule fires, the subset of Graph
// messageRuleActions with clean Sieve equivalents.
type RuleActions struct {
	// MoveToFolder is the opaque ID of the destination folder (Sieve: fileinto).
	MoveToFolder string
	// CopyToFolder files a copy into the folder while keeping the original (Sieve:
	// fileinto :copy).
	CopyToFolder string
	// MarkAsRead sets the \Seen flag (Sieve: setflag, RFC 5232 imap4flags).
	MarkAsRead bool
	// Delete discards the message (Sieve: discard).
	Delete bool
	// ForwardTo forwards a copy to the given addresses, keeping the original (Sieve:
	// redirect with the implicit keep).
	ForwardTo []Address
	// RedirectTo redirects to the given addresses without keeping a local copy
	// (Sieve: redirect).
	RedirectTo []Address
	// StopProcessingRules halts evaluation of later rules (Sieve: stop).
	StopProcessingRules bool
}

// FilterReader is the optional capability to read a mailbox's inbox rules (mail
// filters). Like the other capabilities it is kept separate from Backend so an
// adapter without filter support need not implement it; consumers type-assert:
//
//	if fr, ok := backend.(mail.FilterReader); ok {
//		rules, err := fr.ListRules(ctx)
//	}
//
// It backs Graph's GET .../messageRules and .../messageRules/{id}. Graph nests
// these under a mailFolder (conventionally the inbox), but the backend filter
// mechanism is mailbox-global, so the rules are mailbox-scoped, not per-folder. A
// FilterReader is bound to the same authenticated mailbox identity as its Backend.
type FilterReader interface {
	// ListRules returns the mailbox's rules in evaluation order (lowest Sequence
	// first).
	ListRules(ctx context.Context) ([]MessageRule, error)
	// GetRule returns the rule with the given opaque id, or ErrRuleNotFound.
	GetRule(ctx context.Context, id string) (MessageRule, error)
}

// FilterWriter is the optional capability to create, update, and delete inbox
// rules, backing Graph's POST/PATCH/DELETE .../messageRules. Like FilterReader it
// is type-asserted and mailbox-scoped. A FilterWriter is bound to the same
// authenticated mailbox identity as its Backend.
type FilterWriter interface {
	// CreateRule persists rule (whose ID is empty) and returns it with the
	// backend-assigned opaque ID.
	CreateRule(ctx context.Context, rule MessageRule) (MessageRule, error)
	// UpdateRule replaces the rule with the given id, returning the updated rule, or
	// ErrRuleNotFound when no such rule exists.
	UpdateRule(ctx context.Context, id string, rule MessageRule) (MessageRule, error)
	// DeleteRule removes the rule with the given id, or returns ErrRuleNotFound.
	DeleteRule(ctx context.Context, id string) error
}

// ErrRuleNotFound is returned by FilterReader/FilterWriter when no rule has the
// requested id.
var ErrRuleNotFound = errors.New("mail: message rule not found")

// ErrFiltersUnsupported is returned by a FilterReader/FilterWriter when the backend
// connection cannot manage filters even though it implements the capability — e.g. a
// JMAP session that does not advertise the Sieve capability (RFC 9661). The server
// maps it to 501, the same posture as ErrNoQuota.
var ErrFiltersUnsupported = errors.New("mail: filters not supported by this backend")

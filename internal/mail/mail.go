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
	"time"

	"github.com/hstern/go-mailbox-720/internal/odata"
)

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

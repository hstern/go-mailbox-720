// Package contacts defines the contacts backing-store port: a backend-neutral,
// Graph/JMAP-shaped view of address books and contacts that the server maps
// Microsoft Graph requests onto. The CardDAV adapter (internal/contacts/carddav)
// is the first implementation; a JMAP-contacts adapter can drop in behind the
// same interface later.
//
// Like the mail port (internal/mail) and calendar port (internal/calendar), this
// port holds no contact data of its own — each method round-trips to the
// operator's existing CardDAV server. Address-book and contact IDs are opaque and
// stable, derived from backend identifiers (CardDAV hrefs) so a Graph client can
// round-trip them.
package contacts

import (
	"context"
)

// EmailAddress is a contact's email address with an optional type label (e.g.
// "home" or "work") drawn from the vCard EMAIL property's TYPE parameter.
type EmailAddress struct {
	Address string
	Type    string
}

// Phone is a contact's telephone number with an optional type label (e.g.
// "home", "work", or "cell") drawn from the vCard TEL property's TYPE parameter.
type Phone struct {
	Number string
	Type   string
}

// AddressBook is an address-book collection in Graph/JMAP object shape.
type AddressBook struct {
	ID          string
	Name        string
	Description string
}

// Contact is an address-book entry in Graph/JMAP object shape. Because CardDAV
// returns whole vCards rather than a cheap envelope, list and get operations
// populate the same fields; there is no body to defer.
type Contact struct {
	ID            string
	AddressBookID string
	UID           string // the vCard UID, stable across the contact's lifetime
	DisplayName   string // mapped from the vCard FN (formatted name)
	GivenName     string // the vCard N given-name component
	Surname       string // the vCard N family-name component
	Organization  string // the vCard ORG (organization name)
	Title         string // the vCard TITLE (job title)
	Emails        []EmailAddress
	Phones        []Phone
	Note          string
}

// Backend is the contacts backing-store port. Implementations adapt a concrete
// server (CardDAV first) to this neutral shape. A Backend is bound to a single
// authenticated principal.
//
// First cut: the read paths only. Deferred to their own issues (mirroring the
// mail and calendar ports): change subscriptions / push, delta sync tokens,
// $filter execution, and contact creation / modification.
type Backend interface {
	// ListAddressBooks returns the principal's address-book collections.
	ListAddressBooks(ctx context.Context) ([]AddressBook, error)
	// ListContacts returns the contacts in an address book.
	ListContacts(ctx context.Context, addressBookID string) ([]Contact, error)
	// GetContact returns a single contact by opaque ID.
	GetContact(ctx context.Context, id string) (Contact, error)
	// Close releases the backend connection.
	Close() error
}

// Writer is the optional contact write capability: create, update, and delete.
// It is kept separate from Backend so that a read-only adapter (or the server's
// read-path fakes) need not implement writes, and so that adding writes does not
// disturb Backend's existing implementers. An adapter that supports writes
// implements Writer in addition to Backend; consumers type-assert for it:
//
//	if w, ok := backend.(contacts.Writer); ok {
//		created, err := w.CreateContact(ctx, addressBookID, c)
//	}
//
// A Writer is bound to the same authenticated principal as its Backend.
type Writer interface {
	// CreateContact creates a new contact in the named address book and returns
	// it stamped with its assigned opaque ID (and, when the backend generates
	// one, its UID). The input contact's ID is ignored.
	CreateContact(ctx context.Context, addressBookID string, c Contact) (Contact, error)
	// UpdateContact replaces the contact identified by c.ID with c and returns
	// the stored contact. The opaque c.ID locates the backing resource;
	// AddressBookID is derived from it.
	UpdateContact(ctx context.Context, c Contact) (Contact, error)
	// DeleteContact removes the contact with the given opaque ID. Deleting a
	// contact that does not exist returns a not-found error (mirroring Graph's
	// DELETE semantics); a caller wanting idempotent cleanup can ignore it.
	DeleteContact(ctx context.Context, id string) error
}

// DeltaReader is the optional incremental-sync capability: report the contacts
// in an address book that have changed since a prior point, identified by an
// opaque token (an RFC 6578 sync-token). It is kept separate from Backend (like
// Writer) so that an adapter without delta support, and the server's read-path
// fakes, need not implement it, and so adding it does not disturb Backend's
// existing implementers. An adapter that supports delta implements DeltaReader
// in addition to Backend; consumers type-assert for it:
//
//	if d, ok := backend.(contacts.DeltaReader); ok {
//		changed, next, err := d.Delta(ctx, addressBookID, token)
//	}
//
// This is the backing for Microsoft Graph's GET /me/contacts/delta. A
// DeltaReader is bound to the same authenticated principal as its Backend.
type DeltaReader interface {
	// Delta returns the contacts in the address book changed since the opaque
	// token (an RFC 6578 sync-token). An empty token means initial sync: all
	// current contacts + a fresh token; the next token is fed back next call.
	//
	// changed holds created/updated contacts; removed holds the opaque IDs of
	// contacts the sync-collection reported as deleted (so the handler can emit
	// Graph @removed tombstones). On an initial sync removed is empty.
	Delta(ctx context.Context, addressBookID string, token string) (changed []Contact, removed []string, next string, err error)
}

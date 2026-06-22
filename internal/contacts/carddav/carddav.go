// Package carddav implements the contacts.Backend port against a CardDAV server
// using emersion/go-webdav (the carddav subpackage) for the protocol and
// emersion/go-vcard for vCard parsing. A Client is bound to one authenticated
// CardDAV principal.
//
// First cut: the read paths (address-book discovery, contact listing, single
// contact fetch). Deferred to their own issues (mirroring the mail and calendar
// ports): push notifications, delta sync tokens, $filter execution, and contact
// submission.
package carddav

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"

	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/davauth"
)

// Options configures the CardDAV connection.
type Options struct {
	// HTTPClient performs the underlying requests. When nil, http.DefaultClient
	// is used. Supply a custom client to control TLS, timeouts, or proxies.
	HTTPClient webdav.HTTPClient
	// BearerToken, when non-empty, authenticates every request with
	// Authorization: Bearer (the per-identity path, MB720-44) instead of HTTP
	// Basic — the Dial username and password are then ignored. The token is an
	// exchanged backend-audience access token.
	BearerToken string
}

// Client is a CardDAV-backed contacts.Backend over a single authenticated
// principal.
type Client struct {
	c *gocarddav.Client
}

var _ contacts.Backend = (*Client)(nil)

// Dial builds a CardDAV client for endpoint (the server's CardDAV base URL),
// authenticating every request with HTTP Basic credentials — or, when
// Options.BearerToken is set, with Authorization: Bearer (the per-identity path).
// It does not perform any network I/O itself — discovery happens lazily on the
// first call.
func Dial(endpoint, username, password string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if o.BearerToken != "" {
		httpClient = davauth.BearerHTTPClient(httpClient, o.BearerToken)
	} else {
		httpClient = webdav.HTTPClientWithBasicAuth(httpClient, username, password)
	}

	c, err := gocarddav.NewClient(httpClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("carddav: new client for %s: %w", endpoint, err)
	}
	return &Client{c: c}, nil
}

// Close releases the backend. The CardDAV client holds no persistent connection
// of its own (it rides on net/http connection pooling), so this is a no-op.
func (cl *Client) Close() error {
	return nil
}

// ListAddressBooks discovers the principal's address-book collections via the
// CardDAV addressbook-home-set, then PROPFINDs each address book's metadata.
func (cl *Client) ListAddressBooks(ctx context.Context) ([]contacts.AddressBook, error) {
	principal, err := cl.c.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("carddav: find principal: %w", err)
	}
	homeSet, err := cl.c.FindAddressBookHomeSet(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("carddav: find address book home set: %w", err)
	}
	books, err := cl.c.FindAddressBooks(ctx, homeSet)
	if err != nil {
		return nil, fmt.Errorf("carddav: find address books: %w", err)
	}
	out := make([]contacts.AddressBook, 0, len(books))
	for _, b := range books {
		out = append(out, contacts.AddressBook{
			ID:          addressBookID(b.Path),
			Name:        b.Name,
			Description: b.Description,
		})
	}
	return out, nil
}

// ListContacts lists the contacts in an address book via a CardDAV
// addressbook-query REPORT. Every returned vCard is mapped to a Contact; an
// address object that fails to map is skipped rather than failing the whole
// listing.
func (cl *Client) ListContacts(ctx context.Context, abID string) ([]contacts.Contact, error) {
	abPath, err := decodeAddressBookID(abID)
	if err != nil {
		return nil, err
	}
	query := &gocarddav.AddressBookQuery{
		DataRequest: gocarddav.AddressDataRequest{AllProp: true},
	}
	objs, err := cl.c.QueryAddressBook(ctx, abPath, query)
	if err != nil {
		return nil, fmt.Errorf("carddav: query address book %q: %w", abPath, err)
	}
	var out []contacts.Contact
	for _, obj := range objs {
		if c, ok := contactFromObject(abID, obj.Path, obj.Card); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// GetContact fetches a single address object resource by opaque ID and maps its
// vCard.
func (cl *Client) GetContact(ctx context.Context, id string) (contacts.Contact, error) {
	objectPath, err := decodeContactID(id)
	if err != nil {
		return contacts.Contact{}, err
	}
	obj, err := cl.c.GetAddressObject(ctx, objectPath)
	if err != nil {
		return contacts.Contact{}, fmt.Errorf("carddav: get address object %q: %w", objectPath, err)
	}
	c, ok := contactFromObject(addressBookIDForObject(objectPath), objectPath, obj.Card)
	if !ok {
		return contacts.Contact{}, fmt.Errorf("carddav: address object %s has no vCard", id)
	}
	return c, nil
}

// contactFromObject maps one CardDAV address object's vCard to a neutral
// Contact, stamping the opaque IDs. Reports false when the card is empty (nil or
// no properties), so an unparseable object is skipped rather than yielding a
// blank contact.
func contactFromObject(abID, objectPath string, card vcard.Card) (contacts.Contact, bool) {
	if len(card) == 0 {
		return contacts.Contact{}, false
	}
	c := mapContact(card)
	c.ID = contactID(objectPath)
	c.AddressBookID = abID
	return c, true
}

// mapContact maps a vCard to a neutral Contact. It is best-effort: a missing
// property yields a zero value for that field rather than failing the whole
// contact. ID and AddressBookID are left to the caller.
func mapContact(card vcard.Card) contacts.Contact {
	c := contacts.Contact{
		UID:          card.Value(vcard.FieldUID),
		DisplayName:  card.PreferredValue(vcard.FieldFormattedName),
		Organization: orgName(card.Value(vcard.FieldOrganization)),
		Title:        card.Value(vcard.FieldTitle),
		Note:         card.Value(vcard.FieldNote),
	}
	if n := card.Name(); n != nil {
		c.GivenName = n.GivenName
		c.Surname = n.FamilyName
	}
	for _, f := range card[vcard.FieldEmail] {
		addr := strings.TrimSpace(f.Value)
		if addr == "" {
			continue
		}
		c.Emails = append(c.Emails, contacts.EmailAddress{
			Address: addr,
			Type:    primaryType(f),
		})
	}
	for _, f := range card[vcard.FieldTelephone] {
		num := strings.TrimSpace(f.Value)
		if num == "" {
			continue
		}
		c.Phones = append(c.Phones, contacts.Phone{
			Number: num,
			Type:   primaryType(f),
		})
	}
	return c
}

// orgName extracts the organization name from a vCard ORG value. ORG is a
// structured value whose components (organization name, then organizational
// units) are separated by ";"; only the first component is the organization
// name.
func orgName(org string) string {
	if i := strings.IndexByte(org, ';'); i >= 0 {
		return org[:i]
	}
	return org
}

// primaryType returns a field's first TYPE parameter value (e.g. "home" or
// "work"), or "" when the field carries no TYPE. A vCard field may list several
// types; the first is reported.
func primaryType(f *vcard.Field) string {
	if f == nil {
		return ""
	}
	if types := f.Params.Types(); len(types) > 0 {
		return types[0]
	}
	return ""
}

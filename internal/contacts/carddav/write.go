package carddav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
	"github.com/hstern/go-jscontact"
	jsvcard "github.com/hstern/go-jscontact/vcard"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

var _ contacts.Writer = (*Client)(nil)

// CreateContact builds a vCard from c and PUTs it as a new address object
// resource under the named address-book collection. It mints a fresh vCard UID
// (unless c.UID is already set) and an object path of the form
// "<collection>/<uid>.vcf", then returns the contact stamped with the opaque ID
// that encodes that path (and its UID and AddressBookID).
func (cl *Client) CreateContact(ctx context.Context, abID string, c contacts.Contact) (contacts.Contact, error) {
	abPath, err := decodeAddressBookID(abID)
	if err != nil {
		return contacts.Contact{}, err
	}
	if c.UID == "" {
		c.UID = newUID()
	}
	name, err := contactObjectName(c.UID)
	if err != nil {
		return contacts.Contact{}, err
	}
	objectPath := path.Join(abPath, name)
	// path.Join strips a trailing slash; CardDAV object resources are addressed
	// by their full href, which has no trailing slash, so this is correct.
	if err := cl.putContact(ctx, objectPath, c, nil); err != nil {
		return contacts.Contact{}, fmt.Errorf("carddav: create contact in %q: %w", abPath, err)
	}
	c.ID = contactID(objectPath)
	c.AddressBookID = abID
	return c, nil
}

// UpdateContact overwrites the address object resource identified by c.ID with a
// vCard built from c. The opaque ID decodes to the object path; the contact
// keeps its existing UID, so callers should preserve c.UID across a
// read-modify-write.
func (cl *Client) UpdateContact(ctx context.Context, c contacts.Contact) (contacts.Contact, error) {
	objectPath, err := decodeContactID(c.ID)
	if err != nil {
		return contacts.Contact{}, err
	}
	if err := cl.putContact(ctx, objectPath, c, nil); err != nil {
		return contacts.Contact{}, fmt.Errorf("carddav: update contact %q: %w", objectPath, err)
	}
	c.AddressBookID = addressBookIDForObject(objectPath)
	return c, nil
}

var _ contacts.ConditionalWriter = (*Client)(nil)

// UpdateContactIfMatch overwrites the address object resource identified by c.ID
// with a vCard built from c, but only if the resource's current ETag matches
// ifMatch. It issues a conditional PUT (If-Match: ifMatch) so the CardDAV server
// enforces the precondition atomically — there is no read-modify-write window. A
// failed precondition (the server's 412 Precondition Failed) is translated to
// contacts.ErrPreconditionFailed; the HTTP layer maps that to 412.
func (cl *Client) UpdateContactIfMatch(ctx context.Context, c contacts.Contact, ifMatch string) (contacts.Contact, error) {
	if ifMatch == "" {
		return contacts.Contact{}, fmt.Errorf("carddav: UpdateContactIfMatch requires a non-empty If-Match ETag")
	}
	objectPath, err := decodeContactID(c.ID)
	if err != nil {
		return contacts.Contact{}, err
	}
	opts := &gocarddav.PutAddressObjectOptions{IfMatch: webdav.ConditionalMatch(ifMatch)}
	if err := cl.putContact(ctx, objectPath, c, opts); err != nil {
		if code, ok := webdav.HTTPErrorCode(err); ok && code == http.StatusPreconditionFailed {
			return contacts.Contact{}, contacts.ErrPreconditionFailed
		}
		return contacts.Contact{}, fmt.Errorf("carddav: conditional update contact %q: %w", objectPath, err)
	}
	c.AddressBookID = addressBookIDForObject(objectPath)
	return c, nil
}

// DeleteContact removes the address object resource identified by id via an
// authenticated HTTP DELETE (go-webdav's RemoveAll). Deleting a resource that no
// longer exists returns the server's error (typically a 404) — matching Graph's
// own DELETE semantics; a caller wanting idempotent cleanup can ignore a
// not-found error.
func (cl *Client) DeleteContact(ctx context.Context, id string) error {
	objectPath, err := decodeContactID(id)
	if err != nil {
		return err
	}
	if err := cl.c.RemoveAll(ctx, objectPath); err != nil {
		return fmt.Errorf("carddav: delete contact %q: %w", objectPath, err)
	}
	return nil
}

// putContact encodes c as a vCard and PUTs it at objectPath. A nil opts performs
// an unconditional PUT; a non-nil opts carries an If-Match precondition (see
// UpdateContactIfMatch).
func (cl *Client) putContact(ctx context.Context, objectPath string, c contacts.Contact, opts *gocarddav.PutAddressObjectOptions) error {
	card, err := contactToCard(c)
	if err != nil {
		return err
	}
	if _, err := cl.c.PutAddressObject(ctx, objectPath, card, opts); err != nil {
		return fmt.Errorf("put address object: %w", err)
	}
	return nil
}

// contactToCard builds a version-4 vCard from a neutral Contact, the inverse of
// contactFromObject. It is the write-path counterpart used by CreateContact and
// UpdateContact. The JSContact→vCard field mapping (N, ORG, TITLE, EMAIL, TEL,
// NOTE, TYPE params, …) is delegated to the go-jscontact/vcard bridge
// (RFC 9555); this only guarantees the RFC 6350 §6.2.1 FN by filling the card's
// Name.Full from the Graph-shaped DisplayName fallback when it is empty, then
// sanitises every emitted property value and parameter (the bridge and go-vcard
// neither escape nor strip control characters, so an unsanitised CR/LF or
// structural char could forge a property/parameter line). ToVCard sets
// VERSION:4.0 so go-webdav's PUT carries a conformant body.
func contactToCard(c contacts.Contact) (vcard.Card, error) {
	ensureFormattedName(&c)
	card, err := jsvcard.ToVCard(&c.Card)
	if err != nil {
		return nil, fmt.Errorf("carddav: convert contact to vcard: %w", err)
	}
	sanitizeCard(card)
	return card, nil
}

// ensureFormattedName guarantees the contact carries a name with a non-empty
// Full so the bridge emits the RFC 6350-required FN. When Name.Full is empty it
// is filled from the Graph-shaped DisplayName fallback (assembled given+surname,
// else organization), mirroring the old hand-mapped formattedName.
func ensureFormattedName(c *contacts.Contact) {
	if c.Name != nil && c.Name.Full != "" {
		return
	}
	full := c.DisplayName()
	if full == "" {
		full = c.Organization()
	}
	if full == "" {
		return
	}
	if c.Name == nil {
		c.Name = &jscontact.Name{}
	}
	c.Name.Full = full
}

// sanitizeCard strips injection-unsafe characters from every value and parameter
// of an encoded card. The go-jscontact/vcard bridge passes JSContact values
// through to go-vcard verbatim, and go-vcard's encoder escapes "\", "\n" and ","
// in values but neither escapes nor quotes parameter values — so a value or TYPE
// containing CR/LF (or a structural char in a parameter) could forge a property
// or parameter line on the wire. This keeps the encoded card single-line-per-
// property, the same guarantee the old hand-mapped contactToCard provided.
func sanitizeCard(card vcard.Card) {
	for _, fields := range card {
		for _, f := range fields {
			f.Value = sanitizeValue(f.Value)
			for name, vals := range f.Params {
				for i, v := range vals {
					vals[i] = sanitizeParam(v)
				}
				f.Params[name] = vals
			}
		}
	}
}

// sanitizeValue strips control characters from a vCard property value. go-vcard's
// encoder escapes "\", "\n" and "," but NOT a bare CR (and other control
// characters), which a strict parser could treat as a line break — so a value
// containing CR/LF could otherwise forge a property line. Stripping them keeps
// the encoded card single-line-per-property.
func sanitizeValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
}

// sanitizeParam strips characters unsafe in a vCard parameter value. go-vcard
// neither escapes nor quotes parameter values, so a TYPE containing CR/LF, a
// double quote, or the structural ":" / ";" / "," could break out of the
// parameter and forge a property or parameter.
func sanitizeParam(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`";:,`, r) {
			return -1
		}
		return r
	}, s)
}

// contactObjectName returns the ".vcf" object filename for a UID, rejecting a
// caller-supplied UID that could escape the address-book collection: CreateContact
// joins this onto the collection path, so a UID with a path separator or a "."/
// ".." segment would otherwise write outside the named collection.
func contactObjectName(uid string) (string, error) {
	if uid == "." || uid == ".." || strings.ContainsAny(uid, `/\`) ||
		strings.IndexFunc(uid, func(r rune) bool { return r < 0x20 }) >= 0 {
		return "", fmt.Errorf("carddav: unsafe contact UID %q", uid)
	}
	return uid + ".vcf", nil
}

// newUID mints a fresh vCard UID for a created contact: 16 random bytes hex
// encoded, scoped with this adapter's domain to make it globally unique per
// RFC 6350 §6.7.6. Mirrors caldav.newUID.
func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + "@go-mailbox-720"
}

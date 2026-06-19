package carddav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/emersion/go-vcard"

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
	if err := cl.putContact(ctx, objectPath, c); err != nil {
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
	if err := cl.putContact(ctx, objectPath, c); err != nil {
		return contacts.Contact{}, fmt.Errorf("carddav: update contact %q: %w", objectPath, err)
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

// putContact encodes c as a vCard and PUTs it at objectPath.
func (cl *Client) putContact(ctx context.Context, objectPath string, c contacts.Contact) error {
	card := contactToCard(c)
	if _, err := cl.c.PutAddressObject(ctx, objectPath, card); err != nil {
		return fmt.Errorf("put address object: %w", err)
	}
	return nil
}

// contactToCard builds a version-4 vCard from a neutral Contact, the inverse of
// mapContact. It is the write-path counterpart used by CreateContact and
// UpdateContact. It always emits the FN and UID properties (FN is required by
// RFC 6350 §6.2.1, falling back to the assembled name when DisplayName is
// empty), and a structured N from the given/family-name components; EMAIL, TEL,
// ORG, TITLE, and NOTE are emitted only when present. ToV4 normalises the card
// to vCard 4.0 (setting VERSION) so go-webdav's PUT carries a conformant body.
func contactToCard(c contacts.Contact) vcard.Card {
	card := make(vcard.Card)

	card.SetName(&vcard.Name{
		FamilyName: sanitizeValue(c.Surname),
		GivenName:  sanitizeValue(c.GivenName),
	})

	card.SetValue(vcard.FieldFormattedName, sanitizeValue(formattedName(c)))
	card.SetValue(vcard.FieldUID, sanitizeValue(c.UID))

	for _, e := range c.Emails {
		f := &vcard.Field{Value: sanitizeValue(e.Address)}
		if e.Type != "" {
			f.Params = vcard.Params{vcard.ParamType: []string{sanitizeParam(e.Type)}}
		}
		card.Add(vcard.FieldEmail, f)
	}
	for _, p := range c.Phones {
		f := &vcard.Field{Value: sanitizeValue(p.Number)}
		if p.Type != "" {
			f.Params = vcard.Params{vcard.ParamType: []string{sanitizeParam(p.Type)}}
		}
		card.Add(vcard.FieldTelephone, f)
	}
	if c.Organization != "" {
		card.SetValue(vcard.FieldOrganization, sanitizeValue(c.Organization))
	}
	if c.Title != "" {
		card.SetValue(vcard.FieldTitle, sanitizeValue(c.Title))
	}
	if c.Note != "" {
		card.SetValue(vcard.FieldNote, sanitizeValue(c.Note))
	}

	vcard.ToV4(card)
	return card
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

// formattedName chooses the value for the required FN property: the explicit
// DisplayName when set, otherwise the given and family names joined, otherwise
// the organization. RFC 6350 requires at least one FN, so a non-empty value is
// always produced for a contact carrying any identifying field.
func formattedName(c contacts.Contact) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	switch {
	case c.GivenName != "" && c.Surname != "":
		return c.GivenName + " " + c.Surname
	case c.GivenName != "":
		return c.GivenName
	case c.Surname != "":
		return c.Surname
	default:
		return c.Organization
	}
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

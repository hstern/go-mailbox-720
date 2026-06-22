package contacts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hstern/go-jscontact"
)

// This file holds the shared JSContact (RFC 9553) helper contract the whole
// contacts subsystem maps through. The neutral Contact embeds the full
// jscontact.Card, but the Microsoft Graph contact slice is single-valued and
// flat (one display/given/surname, one company, one job title, a flat email and
// phone list). These helpers project the rich Card onto that Graph-shaped view
// (read side) and build a Card from it (write side), so the CardDAV/JMAP
// adapters and the server Graph layer share one mapping rather than re-deriving
// it per package. vCard↔Card itself is handled by the go-jscontact/vcard bridge.

// --- Name ---

// DisplayName returns the formatted name (Card.Name.Full), falling back to the
// given+surname components joined, mirroring the vCard FN.
func (c Contact) DisplayName() string {
	if c.Name != nil && c.Name.Full != "" {
		return c.Name.Full
	}
	return strings.TrimSpace(c.GivenName() + " " + c.Surname())
}

// GivenName returns the "given" name component, if any.
func (c Contact) GivenName() string { return c.nameComponent("given") }

// Surname returns the "surname" name component, if any.
func (c Contact) Surname() string { return c.nameComponent("surname") }

func (c Contact) nameComponent(kind string) string {
	if c.Name == nil {
		return ""
	}
	for _, comp := range c.Name.Components {
		if comp.Kind == kind {
			return comp.Value
		}
	}
	return ""
}

// SetName sets the JSContact Name from the Graph-shaped display/given/surname
// triple: Full from display, plus "given"/"surname" components. All-empty
// clears Name.
func (c *Contact) SetName(display, given, surname string) {
	n := jscontact.Name{Full: display}
	if given != "" {
		n.Components = append(n.Components, jscontact.NameComponent{Kind: "given", Value: given})
	}
	if surname != "" {
		n.Components = append(n.Components, jscontact.NameComponent{Kind: "surname", Value: surname})
	}
	if n.Full == "" && len(n.Components) == 0 {
		c.Name = nil
		return
	}
	c.Name = &n
}

// --- Single-valued org / title / note (first sorted key) ---

// Organization returns the name of the first organization, if any.
func (c Contact) Organization() string {
	if k := firstKey(c.Organizations); k != "" {
		return c.Organizations[k].Name
	}
	return ""
}

// SetOrganization sets a single organization (keyed "o1"); empty is a no-op.
func (c *Contact) SetOrganization(name string) {
	if name == "" {
		return
	}
	c.Organizations = map[string]jscontact.Organization{"o1": {Name: name}}
}

// Title returns the name of the first title, if any.
func (c Contact) Title() string {
	if k := firstKey(c.Titles); k != "" {
		return c.Titles[k].Name
	}
	return ""
}

// SetTitle sets a single title (keyed "t1"); empty is a no-op.
func (c *Contact) SetTitle(name string) {
	if name == "" {
		return
	}
	c.Titles = map[string]jscontact.Title{"t1": {Name: name}}
}

// Note returns the text of the first note, if any.
func (c Contact) Note() string {
	if k := firstKey(c.Notes); k != "" {
		return c.Notes[k].Note
	}
	return ""
}

// SetNote sets a single note (keyed "n1"); empty is a no-op.
func (c *Contact) SetNote(note string) {
	if note == "" {
		return
	}
	c.Notes = map[string]jscontact.Note{"n1": {Note: note}}
}

// --- Emails / phones (ordered projections of the keyed maps) ---

// EmailList returns the contact's email addresses in deterministic (sorted-key)
// order, skipping empty entries.
func (c Contact) EmailList() []jscontact.EmailAddress {
	var out []jscontact.EmailAddress
	for _, k := range sortedKeys(c.Emails) {
		if e := c.Emails[k]; e.Address != "" {
			out = append(out, e)
		}
	}
	return out
}

// SetEmails replaces the contact's emails, keying them "e1".."eN" for
// deterministic output. Empty input is a no-op.
func (c *Contact) SetEmails(emails []jscontact.EmailAddress) {
	if len(emails) == 0 {
		return
	}
	m := make(map[string]jscontact.EmailAddress, len(emails))
	for i, e := range emails {
		m[fmt.Sprintf("e%d", i+1)] = e
	}
	c.Emails = m
}

// PhoneList returns the contact's phone numbers in deterministic (sorted-key)
// order, skipping empty entries.
func (c Contact) PhoneList() []jscontact.Phone {
	var out []jscontact.Phone
	for _, k := range sortedKeys(c.Phones) {
		if p := c.Phones[k]; p.Number != "" {
			out = append(out, p)
		}
	}
	return out
}

// SetPhones replaces the contact's phones, keying them "p1".."pN". Empty input
// is a no-op.
func (c *Contact) SetPhones(phones []jscontact.Phone) {
	if len(phones) == 0 {
		return
	}
	m := make(map[string]jscontact.Phone, len(phones))
	for i, p := range phones {
		m[fmt.Sprintf("p%d", i+1)] = p
	}
	c.Phones = m
}

// NewEmail builds a JSContact email with an optional vCard/Graph-style type
// label carried as a context. An empty type yields a bare address.
func NewEmail(address, typ string) jscontact.EmailAddress {
	e := jscontact.EmailAddress{Address: address}
	if ctx := contextFromType(typ); ctx != "" {
		e.Contexts = map[string]bool{ctx: true}
	}
	return e
}

// EmailType returns the vCard/Graph-style type label for an email: its first
// context (sorted) mapped back to the vCard spelling, falling back to the label.
func EmailType(e jscontact.EmailAddress) string {
	return typeFromContext(firstContext(e.Contexts, e.Label))
}

// NewPhone builds a JSContact phone. "cell"/"mobile" map to the "mobile"
// feature (RFC 9553 §2.3.3); other non-empty types map to a context.
func NewPhone(number, typ string) jscontact.Phone {
	p := jscontact.Phone{Number: number}
	switch strings.ToLower(typ) {
	case "":
		// no type
	case "cell", "mobile":
		p.Features = map[string]bool{"mobile": true}
	default:
		if ctx := contextFromType(typ); ctx != "" {
			p.Contexts = map[string]bool{ctx: true}
		}
	}
	return p
}

// PhoneType returns the vCard/Graph-style type label for a phone: "cell" when
// the "mobile" feature is set, else the first context (sorted) mapped back to
// the vCard spelling, else the label.
func PhoneType(p jscontact.Phone) string {
	if p.Features["mobile"] {
		return "cell"
	}
	return typeFromContext(firstContext(p.Contexts, p.Label))
}

// contextFromType maps a vCard/Graph-style type label to its JSContact context
// spelling: vCard/Graph "home" is JSContact's "private" context (RFC 9553
// §1.5.1, and the spelling the go-jscontact/vcard bridge emits as vCard
// TYPE=home). Other labels pass through lowercased. This keeps the neutral model
// RFC-correct while the Graph and vCard boundaries keep speaking "home".
func contextFromType(typ string) string {
	switch strings.ToLower(typ) {
	case "":
		return ""
	case "home":
		return "private"
	default:
		return strings.ToLower(typ)
	}
}

// typeFromContext is the inverse of contextFromType: JSContact's "private"
// context surfaces as "home" for the vCard/Graph-facing label so the server's
// phone routing (home→homePhones) and email type labels match what real CardDAV
// vCards (TYPE=home → context "private" via the bridge) carry.
func typeFromContext(ctx string) string {
	if ctx == "private" {
		return "home"
	}
	return ctx
}

// --- shared map helpers ---

func firstContext(ctx map[string]bool, label string) string {
	keys := make([]string, 0, len(ctx))
	for k, v := range ctx {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return label
}

// firstKey returns the lowest-sorted key of m, or "" when m is empty — a stable
// choice for the neutral model's single-valued projections.
func firstKey[V any](m map[string]V) string {
	ks := sortedKeys(m)
	if len(ks) == 0 {
		return ""
	}
	return ks[0]
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

package jmap

import (
	"sort"
	"strings"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// contactFromCard maps a JMAP ContactCard (a JSContact Card plus its JMAP id) onto
// the neutral contact, scoped to addressBookID. The neutral model is a small subset;
// JSContact's keyed maps (emails, phones, …) are flattened in sorted-key order so
// the result is deterministic.
func contactFromCard(cc *contactCard, addressBookID string) contacts.Contact {
	card := &cc.Card
	c := contacts.Contact{
		ID:            string(cc.ID),
		AddressBookID: addressBookID,
		UID:           card.UID,
	}

	if card.Name != nil {
		c.DisplayName = card.Name.Full
		for _, comp := range card.Name.Components {
			switch comp.Kind {
			case "given":
				c.GivenName = comp.Value
			case "surname":
				c.Surname = comp.Value
			}
		}
		if c.DisplayName == "" {
			c.DisplayName = strings.TrimSpace(c.GivenName + " " + c.Surname)
		}
	}

	if k := firstKey(card.Organizations); k != "" {
		c.Organization = card.Organizations[k].Name
	}
	if k := firstKey(card.Titles); k != "" {
		c.Title = card.Titles[k].Name
	}
	if k := firstKey(card.Notes); k != "" {
		c.Note = card.Notes[k].Note
	}

	for _, k := range sortedKeys(card.Emails) {
		if e := card.Emails[k]; e.Address != "" {
			c.Emails = append(c.Emails, contacts.EmailAddress{Address: e.Address})
		}
	}
	for _, k := range sortedKeys(card.Phones) {
		if p := card.Phones[k]; p.Number != "" {
			c.Phones = append(c.Phones, contacts.Phone{Number: p.Number})
		}
	}
	return c
}

// firstKey returns the lowest-sorted key of m, or "" when m is empty — a stable
// choice for the neutral model's single-valued fields (organization, title, note).
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

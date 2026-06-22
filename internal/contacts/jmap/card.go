package jmap

import (
	"sort"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// contactFromCard wraps a JMAP ContactCard (a JSContact Card plus its JMAP id) as
// the neutral contact, scoped to addressBookID. Since contacts.Contact embeds the
// full jscontact.Card, the adapter carries the rich Card through unflattened — the
// single-valued, Graph-shaped projection (DisplayName, EmailList, …) is the
// server's concern, not the adapter's.
func contactFromCard(cc *contactCard, addressBookID string) contacts.Contact {
	return contacts.Contact{
		ID:            string(cc.ID),
		AddressBookID: addressBookID,
		Card:          cc.Card,
	}
}

// firstAddressBookID returns the lowest-sorted key of the JMAP addressBookIds set,
// or "" when empty — a stable choice when resolving a card's owning book.
func firstAddressBookID(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return ""
	}
	return ks[0]
}

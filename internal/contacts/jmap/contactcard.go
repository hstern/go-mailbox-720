// Package jmap implements the contacts backing-store port over JMAP for Contacts
// (RFC 9610). go-jmap (git.sr.ht/~rockorager/go-jmap) provides the protocol core
// but no contacts methods, so this package defines the AddressBook/ContactCard
// method calls; the ContactCard object is a JSContact Card (RFC 9553) modelled by
// github.com/hstern/go-jscontact.
package jmap

import (
	"encoding/json"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	jscontact "github.com/hstern/go-jscontact"
)

// contactsURI is the RFC 9610 JMAP capability URN for contacts.
const contactsURI gojmap.URI = "urn:ietf:params:jmap:contacts"

func init() {
	gojmap.RegisterMethod("AddressBook/get", func() gojmap.MethodResponse { return &addressBookGetResponse{} })
	gojmap.RegisterMethod("ContactCard/query", func() gojmap.MethodResponse { return &cardQueryResponse{} })
	gojmap.RegisterMethod("ContactCard/get", func() gojmap.MethodResponse { return &cardGetResponse{} })
}

// --- AddressBook/get (§2.3) ---

type addressBookGet struct {
	Account    gojmap.ID   `json:"accountId,omitempty"`
	IDs        []gojmap.ID `json:"ids,omitempty"`
	Properties []string    `json:"properties,omitempty"`
}

func (m *addressBookGet) Name() string           { return "AddressBook/get" }
func (m *addressBookGet) Requires() []gojmap.URI { return []gojmap.URI{contactsURI} }

type addressBook struct {
	ID          gojmap.ID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
}

type addressBookGetResponse struct {
	Account  gojmap.ID      `json:"accountId,omitempty"`
	State    string         `json:"state,omitempty"`
	List     []*addressBook `json:"list,omitempty"`
	NotFound []gojmap.ID    `json:"notFound,omitempty"`
}

// --- ContactCard/query (§3.3) ---

type cardQuery struct {
	Account gojmap.ID   `json:"accountId,omitempty"`
	Filter  *cardFilter `json:"filter,omitempty"`
	Limit   uint64      `json:"limit,omitempty"`
}

func (m *cardQuery) Name() string           { return "ContactCard/query" }
func (m *cardQuery) Requires() []gojmap.URI { return []gojmap.URI{contactsURI} }

type cardFilter struct {
	InAddressBook gojmap.ID `json:"inAddressBook,omitempty"`
}

type cardQueryResponse struct {
	Account gojmap.ID   `json:"accountId,omitempty"`
	IDs     []gojmap.ID `json:"ids,omitempty"`
}

// --- ContactCard/get (§3.2) ---

type cardGet struct {
	Account    gojmap.ID   `json:"accountId,omitempty"`
	IDs        []gojmap.ID `json:"ids,omitempty"`
	Properties []string    `json:"properties,omitempty"`
}

func (m *cardGet) Name() string           { return "ContactCard/get" }
func (m *cardGet) Requires() []gojmap.URI { return []gojmap.URI{contactsURI} }

type cardGetResponse struct {
	Account  gojmap.ID      `json:"accountId,omitempty"`
	State    string         `json:"state,omitempty"`
	List     []*contactCard `json:"list,omitempty"`
	NotFound []gojmap.ID    `json:"notFound,omitempty"`
}

// contactCard is a JMAP ContactCard: a JSContact Card (RFC 9553) carrying the JMAP
// object metadata (id, addressBookIds) alongside the JSContact body. The metadata
// and the body are decoded from the same JSON object — the JSContact codec ignores
// the JMAP-only members, so they are pulled out separately here.
type contactCard struct {
	ID             gojmap.ID
	AddressBookIDs map[string]bool
	Card           jscontact.Card
}

func (cc *contactCard) UnmarshalJSON(b []byte) error {
	var meta struct {
		ID             gojmap.ID       `json:"id"`
		AddressBookIDs map[string]bool `json:"addressBookIds"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return err
	}
	cc.ID = meta.ID
	cc.AddressBookIDs = meta.AddressBookIDs
	return json.Unmarshal(b, &cc.Card)
}

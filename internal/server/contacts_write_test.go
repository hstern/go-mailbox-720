package server

import (
	"context"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// writableContactsBackend implements BOTH contacts.Backend and contacts.Writer,
// recording write calls so tests can assert the handler reached the Writer with
// the mapped arguments.
type writableContactsBackend struct {
	fakeContactsBackend

	createdBookID  string
	createdContact contacts.Contact
	updatedContact contacts.Contact
	deletedID      string
}

func (f *writableContactsBackend) CreateContact(_ context.Context, addressBookID string, c contacts.Contact) (contacts.Contact, error) {
	f.createdBookID = addressBookID
	f.createdContact = c
	c.ID = "contact-new" // the backend stamps an opaque ID.
	return c, nil
}

func (f *writableContactsBackend) UpdateContact(_ context.Context, c contacts.Contact) (contacts.Contact, error) {
	f.updatedContact = c
	return c, nil
}

func (f *writableContactsBackend) DeleteContact(_ context.Context, id string) error {
	f.deletedID = id
	return nil
}

func newWritableContactsBackend() *writableContactsBackend {
	return &writableContactsBackend{
		fakeContactsBackend: fakeContactsBackend{
			books: []contacts.AddressBook{{ID: "book-default", Name: "Contacts"}},
		},
	}
}

// seededContact is the current contact the writable backend's GetContact
// returns, used by the PATCH (read-modify-write) tests. Its fields stand in for
// an existing stored record so a partial patch can be checked for leaving them
// intact.
var seededContact = contacts.Contact{
	ID:           "contact-1",
	UID:          "uid-1",
	DisplayName:  "Old Name",
	GivenName:    "Old",
	Surname:      "Name",
	Organization: "Old Inc",
	Title:        "Old Title",
	Emails:       []contacts.EmailAddress{{Address: "old@example.com", Type: "work"}},
	Phones:       []contacts.Phone{{Number: "+1-555-0000", Type: "work"}},
}

// newWritableContactsBackendSeeded returns a writable backend whose GetContact
// resolves seededContact by its ID — the current record a PATCH reads, merges,
// and writes back.
func newWritableContactsBackendSeeded() *writableContactsBackend {
	return &writableContactsBackend{
		fakeContactsBackend: fakeContactsBackend{
			books:    []contacts.AddressBook{{ID: "book-default", Name: "Contacts"}},
			contacts: map[string][]contacts.Contact{"book-default": {seededContact}},
		},
	}
}

// writableContactsProvider hands out a writableContactsBackend (Backend+Writer).
type writableContactsProvider struct {
	backend *writableContactsBackend
}

func (p writableContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return p.backend, nil
}

func TestMeCreateContactsMapsBodyAndCallsWriter(t *testing.T) {
	backend := newWritableContactsBackend()
	h := Handler{contacts: writableContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{
		DisplayName: api.NewOptNilString("Carol Example"),
		GivenName:   api.NewOptNilString("Carol"),
		Surname:     api.NewOptNilString("Example"),
		CompanyName: api.NewOptNilString("Example Inc"),
		JobTitle:    api.NewOptNilString("Manager"),
		EmailAddresses: []api.MicrosoftGraphEmailAddress{{
			Address: api.NewOptNilString("carol@example.com"),
			Name:    api.NewOptNilString("work"),
		}},
		BusinessPhones: []api.NilString{{Value: "+1-555-0123"}},
		MobilePhone:    api.NewOptNilString("+1-555-0456"),
	}

	res, err := h.MeCreateContacts(context.Background(), req)
	if err != nil {
		t.Fatalf("MeCreateContacts: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphContactStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphContactStatusCode", res)
	}
	if ok.StatusCode != 201 {
		t.Errorf("status = %d, want 201", ok.StatusCode)
	}
	if backend.createdBookID != "book-default" {
		t.Errorf("create address-book id = %q, want book-default", backend.createdBookID)
	}
	c := backend.createdContact
	if c.DisplayName != "Carol Example" {
		t.Errorf("mapped display name = %q, want Carol Example", c.DisplayName)
	}
	if c.Organization != "Example Inc" {
		t.Errorf("mapped organization = %q, want Example Inc", c.Organization)
	}
	if c.Title != "Manager" {
		t.Errorf("mapped title = %q, want Manager", c.Title)
	}
	if got := len(c.Emails); got != 1 || c.Emails[0].Address != "carol@example.com" {
		t.Errorf("mapped emails = %+v, want one carol@example.com", c.Emails)
	}
	// Business + mobile phones both map back, with vCard TYPE labels.
	var work, cell bool
	for _, p := range c.Phones {
		switch p.Type {
		case "work":
			work = p.Number == "+1-555-0123"
		case "cell":
			cell = p.Number == "+1-555-0456"
		}
	}
	if !work || !cell {
		t.Errorf("mapped phones = %+v, want work+cell", c.Phones)
	}
	if got := ok.Response.ID.Or(""); got != "contact-new" {
		t.Errorf("returned contact id = %q, want contact-new (backend-stamped)", got)
	}
	if !backend.closed {
		t.Error("backend not closed")
	}
}

func TestMeDeleteContactsCallsWriter(t *testing.T) {
	backend := newWritableContactsBackend()
	h := Handler{contacts: writableContactsProvider{backend: backend}}

	res, err := h.MeDeleteContacts(context.Background(), api.MeDeleteContactsParams{ContactID: "contact-1"})
	if err != nil {
		t.Fatalf("MeDeleteContacts: %v", err)
	}
	if _, ok := res.(*api.MeDeleteContactsNoContent); !ok {
		t.Fatalf("response type = %T, want *MeDeleteContactsNoContent (204)", res)
	}
	if backend.deletedID != "contact-1" {
		t.Errorf("deleted id = %q, want contact-1", backend.deletedID)
	}
}

// A read-only backend (Backend but not Writer) must yield the not-implemented
// sentinel (rendered as a Graph 501) for both create and delete.
func TestMeCreateContactsReadOnlyBackendNotImplemented(t *testing.T) {
	backend := newContactsFixture() // *fakeContactsBackend: Backend only, no Writer
	h := Handler{contacts: fakeContactsProvider{backend: backend}}

	if _, err := h.MeCreateContacts(context.Background(), &api.MicrosoftGraphContact{}); err == nil {
		t.Error("MeCreateContacts on read-only backend: expected error, got nil")
	}
	if _, err := h.MeDeleteContacts(context.Background(), api.MeDeleteContactsParams{ContactID: "contact-1"}); err == nil {
		t.Error("MeDeleteContacts on read-only backend: expected error, got nil")
	}
}

func TestNilContactsProviderWriteNotImplemented(t *testing.T) {
	h := Handler{}
	if _, err := h.MeCreateContacts(context.Background(), &api.MicrosoftGraphContact{}); err == nil {
		t.Error("MeCreateContacts with nil provider: expected error, got nil")
	}
	if _, err := h.MeDeleteContacts(context.Background(), api.MeDeleteContactsParams{ContactID: "x"}); err == nil {
		t.Error("MeDeleteContacts with nil provider: expected error, got nil")
	}
}

// TestMeUpdateContactsPartialMergeLeavesAbsentFields: a PATCH that sets only
// DisplayName overlays that one field, leaving the other names/organization/
// title/emails/phones and the contact's identity (ID/UID) intact —
// read-modify-write, not replace.
func TestMeUpdateContactsPartialMergeLeavesAbsentFields(t *testing.T) {
	backend := newWritableContactsBackendSeeded()
	h := Handler{contacts: writableContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{DisplayName: api.NewOptNilString("New Name")}

	res, err := h.MeUpdateContacts(context.Background(), req, api.MeUpdateContactsParams{ContactID: "contact-1"})
	if err != nil {
		t.Fatalf("MeUpdateContacts: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphContactStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphContactStatusCode", res)
	}
	if ok.StatusCode != 200 {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}

	got := backend.updatedContact
	if got.DisplayName != "New Name" {
		t.Errorf("merged display name = %q, want New Name", got.DisplayName)
	}
	if got.GivenName != "Old" || got.Surname != "Name" {
		t.Errorf("merged name parts = (%q,%q), want (Old,Name) unchanged", got.GivenName, got.Surname)
	}
	if got.Organization != "Old Inc" {
		t.Errorf("merged organization = %q, want Old Inc (unchanged)", got.Organization)
	}
	if got.Title != "Old Title" {
		t.Errorf("merged title = %q, want Old Title (unchanged)", got.Title)
	}
	if n := len(got.Emails); n != 1 || got.Emails[0].Address != "old@example.com" {
		t.Errorf("merged emails = %+v, want the original one (unchanged)", got.Emails)
	}
	if n := len(got.Phones); n != 1 || got.Phones[0].Number != "+1-555-0000" {
		t.Errorf("merged phones = %+v, want the original one (unchanged)", got.Phones)
	}
	if got.ID != "contact-1" || got.UID != "uid-1" {
		t.Errorf("merged identity = (%q,%q), want (contact-1,uid-1) preserved", got.ID, got.UID)
	}
	if !backend.closed {
		t.Error("backend not closed")
	}
}

// TestMeUpdateContactsReadOnlyBackendNotImplemented: a read-only backend (Backend
// but not Writer) yields the not-implemented sentinel (Graph 501).
func TestMeUpdateContactsReadOnlyBackendNotImplemented(t *testing.T) {
	backend := newContactsFixture() // *fakeContactsBackend: Backend only, no Writer
	h := Handler{contacts: fakeContactsProvider{backend: backend}}

	if _, err := h.MeUpdateContacts(context.Background(), &api.MicrosoftGraphContact{}, api.MeUpdateContactsParams{ContactID: "contact-1"}); err == nil {
		t.Error("MeUpdateContacts on read-only backend: expected error, got nil")
	}
}

// TestMeUpdateContactsNilProviderNotImplemented: a nil provider yields 501.
func TestMeUpdateContactsNilProviderNotImplemented(t *testing.T) {
	h := Handler{}
	if _, err := h.MeUpdateContacts(context.Background(), &api.MicrosoftGraphContact{}, api.MeUpdateContactsParams{ContactID: "x"}); err == nil {
		t.Error("MeUpdateContacts with nil provider: expected error, got nil")
	}
}

// TestMeUpdateContactsPartialPhonePatchPreservesOtherKinds: patching only the
// mobile number must not drop the contact's existing work/home phones (the three
// Graph phone fields collapse into one neutral slice, so they merge per-kind).
func TestMeUpdateContactsPartialPhonePatchPreservesOtherKinds(t *testing.T) {
	backend := newWritableContactsBackendSeeded() // seeded with a "work" phone
	h := Handler{contacts: writableContactsProvider{backend: backend}}

	req := &api.MicrosoftGraphContact{MobilePhone: api.NewOptNilString("+1-555-9999")}
	if _, err := h.MeUpdateContacts(context.Background(), req, api.MeUpdateContactsParams{ContactID: seededContact.ID}); err != nil {
		t.Fatalf("MeUpdateContacts: %v", err)
	}
	var work, cell string
	for _, p := range backend.updatedContact.Phones {
		switch p.Type {
		case "work":
			work = p.Number
		case "cell":
			cell = p.Number
		}
	}
	if work != "+1-555-0000" {
		t.Errorf("work phone = %q, want +1-555-0000 (must survive a mobile-only patch)", work)
	}
	if cell != "+1-555-9999" {
		t.Errorf("cell phone = %q, want +1-555-9999", cell)
	}
}

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
	deletedID      string
}

func (f *writableContactsBackend) CreateContact(_ context.Context, addressBookID string, c contacts.Contact) (contacts.Contact, error) {
	f.createdBookID = addressBookID
	f.createdContact = c
	c.ID = "contact-new" // the backend stamps an opaque ID.
	return c, nil
}

func (f *writableContactsBackend) UpdateContact(_ context.Context, c contacts.Contact) (contacts.Contact, error) {
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

package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hstern/go-jscontact"

	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// fakeContactsBackend is an in-memory contacts.Backend returning canned data.
type fakeContactsBackend struct {
	books    []contacts.AddressBook
	contacts map[string][]contacts.Contact // keyed by address-book ID
	closed   bool
}

func (f *fakeContactsBackend) ListAddressBooks(_ context.Context) ([]contacts.AddressBook, error) {
	return f.books, nil
}

func (f *fakeContactsBackend) ListContacts(_ context.Context, addressBookID string) ([]contacts.Contact, error) {
	return f.contacts[addressBookID], nil
}

func (f *fakeContactsBackend) GetContact(_ context.Context, id string) (contacts.Contact, error) {
	for _, cs := range f.contacts {
		for _, c := range cs {
			if c.ID == id {
				return c, nil
			}
		}
	}
	// Match the real adapter, which errors on a missing id rather than returning
	// a zero contact.
	return contacts.Contact{}, fmt.Errorf("contact %q not found", id)
}

func (f *fakeContactsBackend) Close() error {
	f.closed = true
	return nil
}

// fakeContactsProvider hands out one fakeContactsBackend.
type fakeContactsProvider struct {
	backend *fakeContactsBackend
}

func (p fakeContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return p.backend, nil
}

func newContactsFixture() *fakeContactsBackend {
	return &fakeContactsBackend{
		books: []contacts.AddressBook{
			{ID: "book-default", Name: "Contacts"},
			{ID: "book-work", Name: "Work"},
		},
		contacts: map[string][]contacts.Contact{
			"book-default": {newTestContact("contact-1", "Alice Example", "Alice", "Example",
				"Example Inc", "Engineer",
				[]jscontact.EmailAddress{contacts.NewEmail("alice@example.com", "work")},
				[]jscontact.Phone{
					contacts.NewPhone("+1-555-0100", "work"),
					contacts.NewPhone("+1-555-0199", "cell"),
				})},
		},
	}
}

// newTestContact builds a contacts.Contact through the JSContact builders — the
// supported construction path now that the projected fields are read-only
// methods on the embedded Card.
func newTestContact(id, display, given, surname, org, title string, emails []jscontact.EmailAddress, phones []jscontact.Phone) contacts.Contact {
	c := contacts.Contact{ID: id}
	c.SetName(display, given, surname)
	c.SetOrganization(org)
	c.SetTitle(title)
	c.SetEmails(emails)
	c.SetPhones(phones)
	return c
}

func TestMeListContactsMapsGraphContacts(t *testing.T) {
	backend := newContactsFixture()
	h := Handler{contacts: fakeContactsProvider{backend: backend}}

	res, err := h.MeListContacts(context.Background(), api.MeListContactsParams{})
	if err != nil {
		t.Fatalf("MeListContacts: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphContactCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphContactCollectionResponseStatusCode", res)
	}
	if ok.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if got := len(ok.Response.Value); got != 1 {
		t.Fatalf("contact count = %d, want 1", got)
	}
	c := ok.Response.Value[0]
	if got := c.ID.Or(""); got != "contact-1" {
		t.Errorf("id = %q, want contact-1", got)
	}
	if got := c.DisplayName.Or(""); got != "Alice Example" {
		t.Errorf("displayName = %q, want Alice Example", got)
	}
	if got := c.GivenName.Or(""); got != "Alice" {
		t.Errorf("givenName = %q, want Alice", got)
	}
	if got := c.Surname.Or(""); got != "Example" {
		t.Errorf("surname = %q, want Example", got)
	}
	if got := c.CompanyName.Or(""); got != "Example Inc" {
		t.Errorf("companyName = %q, want Example Inc", got)
	}
	if got := c.JobTitle.Or(""); got != "Engineer" {
		t.Errorf("jobTitle = %q, want Engineer", got)
	}
	if got := len(c.EmailAddresses); got != 1 {
		t.Fatalf("email count = %d, want 1", got)
	}
	if got := c.EmailAddresses[0].Address.Or(""); got != "alice@example.com" {
		t.Errorf("email address = %q, want alice@example.com", got)
	}
	if got := len(c.BusinessPhones); got != 1 || c.BusinessPhones[0].Value != "+1-555-0100" {
		t.Errorf("businessPhones = %+v, want [+1-555-0100]", c.BusinessPhones)
	}
	if got := c.MobilePhone.Or(""); got != "+1-555-0199" {
		t.Errorf("mobilePhone = %q, want +1-555-0199", got)
	}
	if !backend.closed {
		t.Errorf("backend not closed")
	}
}

// A principal with no address books yields an empty contact list (200), not a
// query against an empty/invalid address-book id.
func TestMeListContactsNoAddressBooks(t *testing.T) {
	backend := &fakeContactsBackend{} // no address books, no contacts
	h := Handler{contacts: fakeContactsProvider{backend: backend}}

	res, err := h.MeListContacts(context.Background(), api.MeListContactsParams{})
	if err != nil {
		t.Fatalf("MeListContacts: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphContactCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want collection", res)
	}
	if ok.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	if got := len(ok.Response.Value); got != 0 {
		t.Errorf("contact count = %d, want 0", got)
	}
}

func TestMeGetContactsMapsGraphContact(t *testing.T) {
	backend := newContactsFixture()
	h := Handler{contacts: fakeContactsProvider{backend: backend}}

	res, err := h.MeGetContacts(context.Background(), api.MeGetContactsParams{ContactID: "contact-1"})
	if err != nil {
		t.Fatalf("MeGetContacts: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphContactStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want *MicrosoftGraphContactStatusCode", res)
	}
	if got := ok.Response.DisplayName.Or(""); got != "Alice Example" {
		t.Errorf("displayName = %q, want Alice Example", got)
	}
}

// A nil contacts provider must yield the ogen "not implemented" sentinel, which
// the error handler renders as a Graph 501 (see server_test.go for the
// HTTP-level assertion on the mail side).
func TestNilContactsProviderNotImplemented(t *testing.T) {
	h := Handler{}

	if _, err := h.MeListContacts(context.Background(), api.MeListContactsParams{}); err == nil {
		t.Fatal("MeListContacts: expected error, got nil")
	}
	if _, err := h.MeGetContacts(context.Background(), api.MeGetContactsParams{ContactID: "x"}); err == nil {
		t.Fatal("MeGetContacts: expected error, got nil")
	}
}

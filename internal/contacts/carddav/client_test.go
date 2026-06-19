package carddav

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	gocarddav "github.com/emersion/go-webdav/carddav"
)

// The integration tests below stand up an in-process CardDAV server using
// go-webdav's own carddav.Handler wired to a tiny in-memory backend, served over
// httptest. This exercises Client's real network methods (PROPFIND discovery,
// addressbook-query REPORT, GET) end-to-end without any external server.

const (
	testPrincipal   = "/test/"
	testHomeSet     = "/test/contacts/"
	testAddressBook = "/test/contacts/default/"
	testCardPath    = "/test/contacts/default/alice.vcf"
)

const serverCard = `BEGIN:VCARD
VERSION:4.0
UID:alice-uid-1
FN:Alice Gopher
N:Gopher;Alice;;;
ORG:Example Corp;Engineering
EMAIL;TYPE=work:alice@example.com
TEL;TYPE=cell:+1-555-0100
END:VCARD`

// memBackend is a minimal mutable gocarddav.Backend backed by a single address
// book whose address objects live in an in-memory map keyed by object path. It
// is seeded with one card and supports PUT/DELETE so the write round-trip tests
// exercise the Client's real network methods end-to-end. Not safe for concurrent
// use; the httptest server processes one request at a time in these tests.
type memBackend struct {
	cards map[string]vcard.Card
}

// newMemBackend returns a backend seeded with the read-test card at
// testCardPath.
func newMemBackend(t *testing.T) *memBackend {
	t.Helper()
	card, err := vcard.NewDecoder(strings.NewReader(serverCard)).Decode()
	if err != nil {
		t.Fatalf("seed card: %v", err)
	}
	return &memBackend{cards: map[string]vcard.Card{testCardPath: card}}
}

func (*memBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return testPrincipal, nil
}

func (*memBackend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	return testHomeSet, nil
}

func (*memBackend) ListAddressBooks(ctx context.Context) ([]gocarddav.AddressBook, error) {
	return []gocarddav.AddressBook{{
		Path:        testAddressBook,
		Name:        "Default",
		Description: "Default address book",
	}}, nil
}

func (b *memBackend) GetAddressBook(ctx context.Context, path string) (*gocarddav.AddressBook, error) {
	books, _ := b.ListAddressBooks(ctx)
	for i := range books {
		if books[i].Path == path {
			return &books[i], nil
		}
	}
	return nil, webdav.NewHTTPError(404, fmt.Errorf("not found"))
}

func (*memBackend) CreateAddressBook(ctx context.Context, ab *gocarddav.AddressBook) error {
	panic("unused")
}
func (*memBackend) DeleteAddressBook(ctx context.Context, path string) error { panic("unused") }

func (b *memBackend) GetAddressObject(ctx context.Context, path string, req *gocarddav.AddressDataRequest) (*gocarddav.AddressObject, error) {
	card, ok := b.cards[path]
	if !ok {
		return nil, webdav.NewHTTPError(404, fmt.Errorf("not found"))
	}
	return &gocarddav.AddressObject{Path: path, Card: card}, nil
}

func (b *memBackend) ListAddressObjects(ctx context.Context, path string, req *gocarddav.AddressDataRequest) ([]gocarddav.AddressObject, error) {
	out := make([]gocarddav.AddressObject, 0, len(b.cards))
	for p, card := range b.cards {
		out = append(out, gocarddav.AddressObject{Path: p, Card: card})
	}
	return out, nil
}

func (b *memBackend) QueryAddressObjects(ctx context.Context, path string, query *gocarddav.AddressBookQuery) ([]gocarddav.AddressObject, error) {
	return b.ListAddressObjects(ctx, path, &query.DataRequest)
}

func (b *memBackend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *gocarddav.PutAddressObjectOptions) (*gocarddav.AddressObject, error) {
	b.cards[path] = card
	return &gocarddav.AddressObject{Path: path, Card: card}, nil
}

func (b *memBackend) DeleteAddressObject(ctx context.Context, path string) error {
	if _, ok := b.cards[path]; !ok {
		return webdav.NewHTTPError(404, fmt.Errorf("not found"))
	}
	delete(b.cards, path)
	return nil
}

// newTestClient stands up the in-process server and returns a Client pointed at
// its well-known CardDAV URL.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	return newTestClientWithBackend(t, newMemBackend(t))
}

// newTestClientWithBackend stands up the in-process server over the given
// backend and returns a Client pointed at its well-known CardDAV URL, letting a
// caller retain a handle to the backend to assert on stored state.
func newTestClientWithBackend(t *testing.T, b *memBackend) *Client {
	t.Helper()
	h := &gocarddav.Handler{Backend: b}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c, err := gocarddav.NewClient(nil, ts.URL+"/.well-known/carddav")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &Client{c: c}
}

func TestClientListAddressBooks(t *testing.T) {
	cl := newTestClient(t)
	books, err := cl.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d address books, want 1", len(books))
	}
	if books[0].ID != addressBookID(testAddressBook) {
		t.Errorf("ID = %q, want %q", books[0].ID, addressBookID(testAddressBook))
	}
	if books[0].Name != "Default" {
		t.Errorf("Name = %q, want %q", books[0].Name, "Default")
	}
}

func TestClientListContacts(t *testing.T) {
	cl := newTestClient(t)
	abID := addressBookID(testAddressBook)
	got, err := cl.ListContacts(context.Background(), abID)
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d contacts, want 1", len(got))
	}
	c := got[0]
	if c.DisplayName != "Alice Gopher" {
		t.Errorf("DisplayName = %q", c.DisplayName)
	}
	if c.AddressBookID != abID {
		t.Errorf("AddressBookID = %q, want %q", c.AddressBookID, abID)
	}
	if c.ID != contactID(testCardPath) {
		t.Errorf("ID = %q, want %q", c.ID, contactID(testCardPath))
	}
	if len(c.Emails) != 1 || c.Emails[0].Address != "alice@example.com" {
		t.Errorf("Emails = %+v", c.Emails)
	}
}

func TestClientListContactsInvalidID(t *testing.T) {
	cl := newTestClient(t)
	if _, err := cl.ListContacts(context.Background(), "!!!"); err == nil {
		t.Error("ListContacts(invalid id) = nil error, want error")
	}
}

func TestClientGetContact(t *testing.T) {
	cl := newTestClient(t)
	id := contactID(testCardPath)
	c, err := cl.GetContact(context.Background(), id)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if c.ID != id {
		t.Errorf("ID = %q, want %q", c.ID, id)
	}
	if c.UID != "alice-uid-1" {
		t.Errorf("UID = %q, want %q", c.UID, "alice-uid-1")
	}
	if c.AddressBookID != addressBookID(testAddressBook) {
		t.Errorf("AddressBookID = %q, want %q", c.AddressBookID, addressBookID(testAddressBook))
	}
	if len(c.Phones) != 1 || c.Phones[0].Number != "+1-555-0100" {
		t.Errorf("Phones = %+v", c.Phones)
	}
}

func TestClientGetContactInvalidID(t *testing.T) {
	cl := newTestClient(t)
	if _, err := cl.GetContact(context.Background(), "!!!"); err == nil {
		t.Error("GetContact(invalid id) = nil error, want error")
	}
}

func TestClientCloseIsNoop(t *testing.T) {
	cl := newTestClient(t)
	if err := cl.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

package carddav

import (
	"strings"
	"testing"

	"github.com/emersion/go-vcard"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// decodeCard parses a single vCard fixture. go-vcard tolerates LF line endings,
// but the fixtures here use plain \n for readability.
func decodeCard(t *testing.T, s string) vcard.Card {
	t.Helper()
	card, err := vcard.NewDecoder(strings.NewReader(s)).Decode()
	if err != nil {
		t.Fatalf("decode card: %v", err)
	}
	return card
}

const fullCard = `BEGIN:VCARD
VERSION:4.0
UID:urn:uuid:4fbe8971-0bc3-424c-9c26-36c3e1eff6b1
FN:Alice Gopher
N:Gopher;Alice;Q;Dr.;PhD
ORG:Example Corp;Engineering
TITLE:Principal Engineer
EMAIL;TYPE=work:alice@example.com
EMAIL;TYPE=home:alice@home.example
TEL;TYPE=cell:+1-555-0100
TEL;TYPE=work:+1-555-0199
NOTE:Met at the Go conference.
END:VCARD
`

func TestMapContactFull(t *testing.T) {
	c := mapContact(decodeCard(t, fullCard))

	if c.UID != "urn:uuid:4fbe8971-0bc3-424c-9c26-36c3e1eff6b1" {
		t.Errorf("UID = %q", c.UID)
	}
	if c.DisplayName != "Alice Gopher" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName, "Alice Gopher")
	}
	if c.GivenName != "Alice" {
		t.Errorf("GivenName = %q, want %q", c.GivenName, "Alice")
	}
	if c.Surname != "Gopher" {
		t.Errorf("Surname = %q, want %q", c.Surname, "Gopher")
	}
	if c.Organization != "Example Corp" {
		t.Errorf("Organization = %q, want %q (first ORG component only)", c.Organization, "Example Corp")
	}
	if c.Title != "Principal Engineer" {
		t.Errorf("Title = %q", c.Title)
	}
	if c.Note != "Met at the Go conference." {
		t.Errorf("Note = %q", c.Note)
	}

	wantEmails := []contacts.EmailAddress{
		{Address: "alice@example.com", Type: "work"},
		{Address: "alice@home.example", Type: "home"},
	}
	if len(c.Emails) != len(wantEmails) {
		t.Fatalf("got %d emails, want %d", len(c.Emails), len(wantEmails))
	}
	for i, want := range wantEmails {
		if c.Emails[i] != want {
			t.Errorf("Emails[%d] = %+v, want %+v", i, c.Emails[i], want)
		}
	}

	wantPhones := []contacts.Phone{
		{Number: "+1-555-0100", Type: "cell"},
		{Number: "+1-555-0199", Type: "work"},
	}
	if len(c.Phones) != len(wantPhones) {
		t.Fatalf("got %d phones, want %d", len(c.Phones), len(wantPhones))
	}
	for i, want := range wantPhones {
		if c.Phones[i] != want {
			t.Errorf("Phones[%d] = %+v, want %+v", i, c.Phones[i], want)
		}
	}
}

// A minimal card carrying only FN must map cleanly: every optional field maps to
// its zero value rather than panicking or erroring.
const minimalCard = `BEGIN:VCARD
VERSION:4.0
FN:Just A Name
END:VCARD
`

func TestMapContactMinimal(t *testing.T) {
	c := mapContact(decodeCard(t, minimalCard))

	if c.DisplayName != "Just A Name" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName, "Just A Name")
	}
	if c.GivenName != "" || c.Surname != "" {
		t.Errorf("name components = %q/%q, want empty (no N property)", c.GivenName, c.Surname)
	}
	if c.UID != "" || c.Organization != "" || c.Title != "" || c.Note != "" {
		t.Errorf("expected empty optional fields, got UID=%q Org=%q Title=%q Note=%q",
			c.UID, c.Organization, c.Title, c.Note)
	}
	if len(c.Emails) != 0 {
		t.Errorf("Emails = %+v, want none", c.Emails)
	}
	if len(c.Phones) != 0 {
		t.Errorf("Phones = %+v, want none", c.Phones)
	}
}

// An EMAIL or TEL with no TYPE parameter maps to an empty Type, not a crash.
const untypedCard = `BEGIN:VCARD
VERSION:4.0
FN:No Types
EMAIL:plain@example.com
TEL:+1-555-0000
END:VCARD
`

func TestMapContactUntypedEmailAndPhone(t *testing.T) {
	c := mapContact(decodeCard(t, untypedCard))

	if len(c.Emails) != 1 || c.Emails[0] != (contacts.EmailAddress{Address: "plain@example.com"}) {
		t.Errorf("Emails = %+v, want one untyped address", c.Emails)
	}
	if len(c.Phones) != 1 || c.Phones[0] != (contacts.Phone{Number: "+1-555-0000"}) {
		t.Errorf("Phones = %+v, want one untyped number", c.Phones)
	}
}

func TestOrgName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Example Corp;Engineering;Backend", "Example Corp"},
		{"Solo Org", "Solo Org"},
		{"", ""},
		{";unit-only", ""},
	}
	for _, tt := range tests {
		if got := orgName(tt.in); got != tt.want {
			t.Errorf("orgName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestContactFromObjectSetsIDs(t *testing.T) {
	card := decodeCard(t, fullCard)
	objectPath := "/addressbooks/alice/contacts/alice.vcf"
	abID := addressBookID("/addressbooks/alice/contacts/")

	c, ok := contactFromObject(abID, objectPath, card)
	if !ok {
		t.Fatal("contactFromObject returned ok=false")
	}
	if c.ID != contactID(objectPath) {
		t.Errorf("ID = %q, want %q", c.ID, contactID(objectPath))
	}
	if c.AddressBookID != abID {
		t.Errorf("AddressBookID = %q, want %q", c.AddressBookID, abID)
	}
}

func TestContactFromObjectEmptyCard(t *testing.T) {
	if _, ok := contactFromObject("ab", "/p.vcf", nil); ok {
		t.Error("contactFromObject(nil card) ok = true, want false")
	}
	if _, ok := contactFromObject("ab", "/p.vcf", vcard.Card{}); ok {
		t.Error("contactFromObject(empty card) ok = true, want false")
	}
}

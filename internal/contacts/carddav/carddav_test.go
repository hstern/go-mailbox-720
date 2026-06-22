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

// mapCard runs a vCard through the read-path bridge (the same path
// contactFromObject uses) and returns the neutral Contact, so the mapping tests
// can assert the Graph-shaped projections.
func mapCard(t *testing.T, s string) contacts.Contact {
	t.Helper()
	c, ok := contactFromObject("ab", "/p.vcf", decodeCard(t, s))
	if !ok {
		t.Fatal("contactFromObject returned ok=false")
	}
	return c
}

func TestMapContactFull(t *testing.T) {
	c := mapCard(t, fullCard)

	if c.UID != "urn:uuid:4fbe8971-0bc3-424c-9c26-36c3e1eff6b1" {
		t.Errorf("UID = %q", c.UID)
	}
	if c.DisplayName() != "Alice Gopher" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName(), "Alice Gopher")
	}
	if c.GivenName() != "Alice" {
		t.Errorf("GivenName = %q, want %q", c.GivenName(), "Alice")
	}
	if c.Surname() != "Gopher" {
		t.Errorf("Surname = %q, want %q", c.Surname(), "Gopher")
	}
	if c.Organization() != "Example Corp" {
		t.Errorf("Organization = %q, want %q (first ORG component only)", c.Organization(), "Example Corp")
	}
	if c.Title() != "Principal Engineer" {
		t.Errorf("Title = %q", c.Title())
	}
	if c.Note() != "Met at the Go conference." {
		t.Errorf("Note = %q", c.Note())
	}

	// The go-jscontact/vcard bridge (RFC 9555) maps EMAIL/TEL TYPE params to
	// JSContact contexts/features: TYPE=work→context "work", TYPE=home→context
	// "private" (RFC 9553's private context is the JSContact spelling of vCard
	// "home"), TYPE=cell→feature "mobile". The contacts helpers map the labels
	// back to the vCard/Graph spelling, so EmailType/PhoneType report "work",
	// "home", and "cell" respectively — the private↔home translation is internal.
	type tl struct{ addr, typ string }
	wantEmails := []tl{
		{"alice@example.com", "work"},
		{"alice@home.example", "home"},
	}
	emails := c.EmailList()
	if len(emails) != len(wantEmails) {
		t.Fatalf("got %d emails, want %d", len(emails), len(wantEmails))
	}
	for i, want := range wantEmails {
		if emails[i].Address != want.addr || contacts.EmailType(emails[i]) != want.typ {
			t.Errorf("Emails[%d] = {%q, %q}, want %+v", i, emails[i].Address, contacts.EmailType(emails[i]), want)
		}
	}

	wantPhones := []tl{
		{"+1-555-0100", "cell"},
		{"+1-555-0199", "work"},
	}
	phones := c.PhoneList()
	if len(phones) != len(wantPhones) {
		t.Fatalf("got %d phones, want %d", len(phones), len(wantPhones))
	}
	for i, want := range wantPhones {
		if phones[i].Number != want.addr || contacts.PhoneType(phones[i]) != want.typ {
			t.Errorf("Phones[%d] = {%q, %q}, want %+v", i, phones[i].Number, contacts.PhoneType(phones[i]), want)
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
	c := mapCard(t, minimalCard)

	if c.DisplayName() != "Just A Name" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName(), "Just A Name")
	}
	if c.GivenName() != "" || c.Surname() != "" {
		t.Errorf("name components = %q/%q, want empty (no N property)", c.GivenName(), c.Surname())
	}
	if c.UID != "" || c.Organization() != "" || c.Title() != "" || c.Note() != "" {
		t.Errorf("expected empty optional fields, got UID=%q Org=%q Title=%q Note=%q",
			c.UID, c.Organization(), c.Title(), c.Note())
	}
	if len(c.EmailList()) != 0 {
		t.Errorf("Emails = %+v, want none", c.EmailList())
	}
	if len(c.PhoneList()) != 0 {
		t.Errorf("Phones = %+v, want none", c.PhoneList())
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
	c := mapCard(t, untypedCard)

	emails := c.EmailList()
	if len(emails) != 1 || emails[0].Address != "plain@example.com" || contacts.EmailType(emails[0]) != "" {
		t.Errorf("Emails = %+v, want one untyped address", emails)
	}
	phones := c.PhoneList()
	if len(phones) != 1 || phones[0].Number != "+1-555-0000" || contacts.PhoneType(phones[0]) != "" {
		t.Errorf("Phones = %+v, want one untyped number", phones)
	}
}

// TestMapContactFirstOrgComponentOnly checks that a structured ORG value
// (organization name plus organizational units, ";"-separated) projects to just
// the organization name through the bridge's Organization mapping.
func TestMapContactFirstOrgComponentOnly(t *testing.T) {
	const card = `BEGIN:VCARD
VERSION:4.0
FN:Org Test
ORG:Example Corp;Engineering;Backend
END:VCARD
`
	if got := mapCard(t, card).Organization(); got != "Example Corp" {
		t.Errorf("Organization = %q, want %q (first ORG component only)", got, "Example Corp")
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

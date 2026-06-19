package carddav

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// TestContactToCardRoundTrip checks that building a vCard from a Contact and
// mapping it back through the read path preserves the contact's identifying
// fields. contactToCard is the inverse of mapContact; this asserts the two stay
// in step.
func TestContactToCardRoundTrip(t *testing.T) {
	in := contacts.Contact{
		UID:          "bob-uid-1",
		DisplayName:  "Bob Builder",
		GivenName:    "Bob",
		Surname:      "Builder",
		Organization: "Construction Co",
		Title:        "Foreman",
		Note:         "met at conference",
		Emails: []contacts.EmailAddress{
			{Address: "bob@example.com", Type: "work"},
			{Address: "bob@home.example", Type: "home"},
		},
		Phones: []contacts.Phone{
			{Number: "+1-555-0101", Type: "cell"},
		},
	}

	card := contactToCard(in)
	got := mapContact(card)

	if got.UID != in.UID {
		t.Errorf("UID = %q, want %q", got.UID, in.UID)
	}
	if got.DisplayName != in.DisplayName {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, in.DisplayName)
	}
	if got.GivenName != in.GivenName {
		t.Errorf("GivenName = %q, want %q", got.GivenName, in.GivenName)
	}
	if got.Surname != in.Surname {
		t.Errorf("Surname = %q, want %q", got.Surname, in.Surname)
	}
	if got.Organization != in.Organization {
		t.Errorf("Organization = %q, want %q", got.Organization, in.Organization)
	}
	if got.Title != in.Title {
		t.Errorf("Title = %q, want %q", got.Title, in.Title)
	}
	if got.Note != in.Note {
		t.Errorf("Note = %q, want %q", got.Note, in.Note)
	}
	if len(got.Emails) != 2 {
		t.Fatalf("Emails = %+v, want 2", got.Emails)
	}
	if got.Emails[0].Address != "bob@example.com" || got.Emails[0].Type != "work" {
		t.Errorf("Emails[0] = %+v", got.Emails[0])
	}
	if got.Emails[1].Address != "bob@home.example" || got.Emails[1].Type != "home" {
		t.Errorf("Emails[1] = %+v", got.Emails[1])
	}
	if len(got.Phones) != 1 || got.Phones[0].Number != "+1-555-0101" || got.Phones[0].Type != "cell" {
		t.Errorf("Phones = %+v", got.Phones)
	}
}

// TestContactToCardFormattedNameFallback checks that a contact lacking an
// explicit DisplayName still produces an FN (required by RFC 6350) assembled
// from the name components.
func TestContactToCardFormattedNameFallback(t *testing.T) {
	card := contactToCard(contacts.Contact{GivenName: "Carol", Surname: "Danvers"})
	if got := mapContact(card).DisplayName; got != "Carol Danvers" {
		t.Errorf("DisplayName = %q, want %q", got, "Carol Danvers")
	}
}

// TestCreateContactMintsUID checks that CreateContact assigns a UID when the
// input contact has none, and derives the opaque ID from the addressbook path
// and that UID.
func TestCreateContactMintsUID(t *testing.T) {
	cl := newTestClient(t)
	abID := addressBookID(testAddressBook)

	created, err := cl.CreateContact(context.Background(), abID, contacts.Contact{
		DisplayName: "Dave Lister",
		GivenName:   "Dave",
		Surname:     "Lister",
	})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if created.UID == "" {
		t.Fatal("CreateContact did not mint a UID")
	}
	if created.AddressBookID != abID {
		t.Errorf("AddressBookID = %q, want %q", created.AddressBookID, abID)
	}
	wantID := contactID(testAddressBook + created.UID + ".vcf")
	if created.ID != wantID {
		t.Errorf("ID = %q, want %q", created.ID, wantID)
	}
}

// TestCreateContactInvalidAddressBookID checks the ID-decode error path.
func TestCreateContactInvalidAddressBookID(t *testing.T) {
	cl := newTestClient(t)
	if _, err := cl.CreateContact(context.Background(), "!!!", contacts.Contact{}); err == nil {
		t.Error("CreateContact(invalid abID) = nil error, want error")
	}
}

// TestUpdateContactInvalidID checks the ID-decode error path.
func TestUpdateContactInvalidID(t *testing.T) {
	cl := newTestClient(t)
	if _, err := cl.UpdateContact(context.Background(), contacts.Contact{ID: "!!!"}); err == nil {
		t.Error("UpdateContact(invalid id) = nil error, want error")
	}
}

// TestDeleteContactInvalidID checks the ID-decode error path.
func TestDeleteContactInvalidID(t *testing.T) {
	cl := newTestClient(t)
	if err := cl.DeleteContact(context.Background(), "!!!"); err == nil {
		t.Error("DeleteContact(invalid id) = nil error, want error")
	}
}

// TestWriteRoundTrip drives the full create/read/update/read/delete cycle
// against the in-process CardDAV server, asserting each step is observable
// through the read path.
func TestWriteRoundTrip(t *testing.T) {
	ctx := context.Background()
	cl := newTestClient(t)
	abID := addressBookID(testAddressBook)

	// Create.
	created, err := cl.CreateContact(ctx, abID, contacts.Contact{
		UID:         "eve-uid-1",
		DisplayName: "Eve Online",
		GivenName:   "Eve",
		Surname:     "Online",
		Emails:      []contacts.EmailAddress{{Address: "eve@example.com", Type: "work"}},
		Phones:      []contacts.Phone{{Number: "+1-555-0102", Type: "cell"}},
	})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}

	// ListContacts now sees the seeded card plus the new one.
	list, err := cl.ListContacts(ctx, abID)
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListContacts after create = %d contacts, want 2", len(list))
	}

	// GetContact reads the new contact back.
	got, err := cl.GetContact(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetContact after create: %v", err)
	}
	if got.DisplayName != "Eve Online" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Eve Online")
	}
	if len(got.Emails) != 1 || got.Emails[0].Address != "eve@example.com" {
		t.Errorf("Emails = %+v", got.Emails)
	}
	if len(got.Phones) != 1 || got.Phones[0].Number != "+1-555-0102" {
		t.Errorf("Phones = %+v", got.Phones)
	}

	// Update a field and confirm a re-read reflects it. Preserve UID across the
	// read-modify-write, as the doc instructs.
	got.DisplayName = "Eve Updated"
	updated, err := cl.UpdateContact(ctx, got)
	if err != nil {
		t.Fatalf("UpdateContact: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("UpdateContact ID = %q, want %q (stable)", updated.ID, created.ID)
	}
	if updated.AddressBookID != abID {
		t.Errorf("UpdateContact AddressBookID = %q, want %q", updated.AddressBookID, abID)
	}
	reread, err := cl.GetContact(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetContact after update: %v", err)
	}
	if reread.DisplayName != "Eve Updated" {
		t.Errorf("DisplayName after update = %q, want %q", reread.DisplayName, "Eve Updated")
	}

	// Delete and confirm it is gone.
	if err := cl.DeleteContact(ctx, created.ID); err != nil {
		t.Fatalf("DeleteContact: %v", err)
	}
	if _, err := cl.GetContact(ctx, created.ID); err == nil {
		t.Error("GetContact after delete = nil error, want error")
	}
	list, err = cl.ListContacts(ctx, abID)
	if err != nil {
		t.Fatalf("ListContacts after delete: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListContacts after delete = %d contacts, want 1", len(list))
	}
}

// TestContactObjectNameRejectsUnsafe guards the write-path: a caller-supplied UID
// must not escape the address-book collection via the object filename.
func TestContactObjectNameRejectsUnsafe(t *testing.T) {
	for _, uid := range []string{"../../evil", "a/b", "..", ".", `x\y`, "has\x00null", "line\r\nbreak"} {
		if _, err := contactObjectName(uid); err == nil {
			t.Errorf("contactObjectName(%q) = nil, want rejection", uid)
		}
	}
	for _, uid := range []string{"abc123@go-mailbox-720", "simple-uid"} {
		name, err := contactObjectName(uid)
		if err != nil {
			t.Errorf("contactObjectName(%q) = %v, want ok", uid, err)
		}
		if name != uid+".vcf" {
			t.Errorf("contactObjectName(%q) = %q, want %q", uid, name, uid+".vcf")
		}
	}
}

// TestContactToCardSanitizes guards against vCard injection: CR/LF in a property
// value (Note) or structural chars in a parameter value (Email TYPE) must not
// forge a property/parameter line, since go-vcard escapes neither.
func TestContactToCardSanitizes(t *testing.T) {
	c := contacts.Contact{
		UID:         "uid-1",
		DisplayName: "Bob",
		Note:        "evil\r\nX-INJECT:1",
		Emails:      []contacts.EmailAddress{{Address: "bob@example.com", Type: "x\r\nFN:Forged"}},
	}
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(contactToCard(c)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := buf.String()
	for _, line := range strings.Split(out, "\r\n") {
		if strings.ContainsAny(line, "\r\n") {
			t.Errorf("raw control char survived on a line: %q", line)
		}
		if strings.HasPrefix(line, "X-INJECT") || strings.HasPrefix(line, "FN:Forged") {
			t.Errorf("forged line injected: %q\nfull:\n%s", line, out)
		}
	}
}

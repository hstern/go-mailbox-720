package carddav

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/hstern/go-jscontact"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// TestContactToCardRoundTrip checks that building a vCard from a Contact and
// mapping it back through the read path preserves the contact's identifying
// fields. contactToCard is the inverse of contactFromObject (both routed through
// the go-jscontact/vcard bridge); this asserts the two stay in step.
func TestContactToCardRoundTrip(t *testing.T) {
	var in contacts.Contact
	in.UID = "bob-uid-1"
	in.SetName("Bob Builder", "Bob", "Builder")
	in.SetOrganization("Construction Co")
	in.SetTitle("Foreman")
	in.SetNote("met at conference")
	// "work" and "home" round-trip through the bridge as vCard TYPE=work /
	// TYPE=home (RFC 9555); the contacts helpers map "home"→JSContact context
	// "private" on the way in and back to "home" on the way out, so both survive.
	in.SetEmails([]jscontact.EmailAddress{
		contacts.NewEmail("bob@example.com", "work"),
		contacts.NewEmail("bob@home.example", "home"),
	})
	// Include a "home" phone to lock the home↔private round trip through the real
	// bridge end-to-end: NewPhone stores context "private", ToVCard emits
	// TEL;TYPE=home, and reading it back must surface as "home" again (so the
	// server routes it to homePhones, not businessPhones).
	in.SetPhones([]jscontact.Phone{
		contacts.NewPhone("+1-555-0101", "cell"),
		contacts.NewPhone("+1-555-0102", "home"),
	})

	card, err := contactToCard(in)
	if err != nil {
		t.Fatalf("contactToCard: %v", err)
	}
	got, ok := contactFromObject("ab", "/p.vcf", "", card)
	if !ok {
		t.Fatal("contactFromObject returned ok=false")
	}

	if got.UID != in.UID {
		t.Errorf("UID = %q, want %q", got.UID, in.UID)
	}
	if got.DisplayName() != in.DisplayName() {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName(), in.DisplayName())
	}
	if got.GivenName() != in.GivenName() {
		t.Errorf("GivenName = %q, want %q", got.GivenName(), in.GivenName())
	}
	if got.Surname() != in.Surname() {
		t.Errorf("Surname = %q, want %q", got.Surname(), in.Surname())
	}
	if got.Organization() != in.Organization() {
		t.Errorf("Organization = %q, want %q", got.Organization(), in.Organization())
	}
	if got.Title() != in.Title() {
		t.Errorf("Title = %q, want %q", got.Title(), in.Title())
	}
	if got.Note() != in.Note() {
		t.Errorf("Note = %q, want %q", got.Note(), in.Note())
	}
	emails := got.EmailList()
	if len(emails) != 2 {
		t.Fatalf("Emails = %+v, want 2", emails)
	}
	if emails[0].Address != "bob@example.com" || contacts.EmailType(emails[0]) != "work" {
		t.Errorf("Emails[0] = {%q, %q}", emails[0].Address, contacts.EmailType(emails[0]))
	}
	if emails[1].Address != "bob@home.example" || contacts.EmailType(emails[1]) != "home" {
		t.Errorf("Emails[1] = {%q, %q}", emails[1].Address, contacts.EmailType(emails[1]))
	}
	phones := got.PhoneList()
	if len(phones) != 2 {
		t.Fatalf("Phones = %+v, want 2", phones)
	}
	if phones[0].Number != "+1-555-0101" || contacts.PhoneType(phones[0]) != "cell" {
		t.Errorf("Phones[0] = {%q, %q}, want cell", phones[0].Number, contacts.PhoneType(phones[0]))
	}
	if phones[1].Number != "+1-555-0102" || contacts.PhoneType(phones[1]) != "home" {
		t.Errorf("Phones[1] = {%q, %q}, want home", phones[1].Number, contacts.PhoneType(phones[1]))
	}
}

// TestContactToCardFormattedNameFallback checks that a contact lacking an
// explicit DisplayName still produces an FN (required by RFC 6350) assembled
// from the name components.
func TestContactToCardFormattedNameFallback(t *testing.T) {
	var c contacts.Contact
	c.SetName("", "Carol", "Danvers")
	card, err := contactToCard(c)
	if err != nil {
		t.Fatalf("contactToCard: %v", err)
	}
	got, ok := contactFromObject("ab", "/p.vcf", "", card)
	if !ok {
		t.Fatal("contactFromObject returned ok=false")
	}
	if got.DisplayName() != "Carol Danvers" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName(), "Carol Danvers")
	}
}

// TestCreateContactMintsUID checks that CreateContact assigns a UID when the
// input contact has none, and derives the opaque ID from the addressbook path
// and that UID.
func TestCreateContactMintsUID(t *testing.T) {
	cl := newTestClient(t)
	abID := addressBookID(testAddressBook)

	var in contacts.Contact
	in.SetName("Dave Lister", "Dave", "Lister")
	created, err := cl.CreateContact(context.Background(), abID, in)
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
	var in contacts.Contact
	in.UID = "eve-uid-1"
	in.SetName("Eve Online", "Eve", "Online")
	in.SetEmails([]jscontact.EmailAddress{contacts.NewEmail("eve@example.com", "work")})
	in.SetPhones([]jscontact.Phone{contacts.NewPhone("+1-555-0102", "cell")})
	created, err := cl.CreateContact(ctx, abID, in)
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
	if got.DisplayName() != "Eve Online" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName(), "Eve Online")
	}
	if emails := got.EmailList(); len(emails) != 1 || emails[0].Address != "eve@example.com" {
		t.Errorf("Emails = %+v", emails)
	}
	if phones := got.PhoneList(); len(phones) != 1 || phones[0].Number != "+1-555-0102" {
		t.Errorf("Phones = %+v", phones)
	}

	// Update a field and confirm a re-read reflects it. Preserve UID across the
	// read-modify-write, as the doc instructs.
	got.SetName("Eve Updated", got.GivenName(), got.Surname())
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
	if reread.DisplayName() != "Eve Updated" {
		t.Errorf("DisplayName after update = %q, want %q", reread.DisplayName(), "Eve Updated")
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
// value must not forge a property line, since neither the go-jscontact/vcard
// bridge nor go-vcard strips control characters. contactToCard sanitises the
// bridge's output (sanitizeCard) before the card reaches the wire.
func TestContactToCardSanitizes(t *testing.T) {
	var c contacts.Contact
	c.UID = "uid-1"
	c.SetName("Bob", "", "")
	c.SetNote("evil\r\nX-INJECT:1")
	card, err := contactToCard(c)
	if err != nil {
		t.Fatalf("contactToCard: %v", err)
	}
	var buf bytes.Buffer
	if err := vcard.NewEncoder(&buf).Encode(card); err != nil {
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

// TestSanitizeParamStripsStructuralChars unit-tests the parameter sanitiser
// directly: although the bridge currently emits only fixed-safe TYPE/PREF
// parameters (so a forged parameter can't reach the wire through a JSContact
// field today), sanitizeCard still defends every emitted parameter, and this
// pins that stripping behaviour.
func TestSanitizeParamStripsStructuralChars(t *testing.T) {
	if got := sanitizeParam("x\r\nFN:Forged"); strings.ContainsAny(got, "\r\n;:,\"") {
		t.Errorf("sanitizeParam left a structural char: %q", got)
	}
}

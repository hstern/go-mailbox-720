package e2e

// Radicale (CardDAV) is the contacts backend of the dumb-backend tier. The same
// Radicale container that serves CalDAV also serves CardDAV, so this file only
// adds the contacts-collection provisioning: it MKCOLs one address-book
// collection under the principal (/test/) and seeds it with one vCard via raw
// authenticated CardDAV requests. mailboxd's CardDAV port discovers the
// collection through the addressbook-home-set and serves GET /me/contacts from
// it. As with the calendar half this reuses the recipe proven by
// internal/contacts/carddav, since this black-box module cannot import internal/.

import (
	"net/http"
	"testing"
)

const (
	// addressBookName / addressBookSlug are the display name and URL segment of
	// the seeded address-book collection (created under the principal /test/).
	// The handler's /me/contacts resolves the principal's first (default) book,
	// so a single collection makes the seeded contact the one returned.
	addressBookName = "Contacts"
	addressBookSlug = "contacts"

	contactFullName = "Carol Contact"
	contactEmail    = "carol@example.com"

	// mkAddressBookBody requests an address-book collection. MKCOL with an
	// addressbook resourcetype is the WebDAV/CardDAV extended-MKCOL form
	// (RFC 5689 / RFC 6352 §5.2) and is issued as a raw request — no go-webdav
	// dependency.
	mkAddressBookBody = `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
    <D:displayname>` + addressBookName + `</D:displayname>
  </D:prop></D:set>
</D:mkcol>`
)

// contactVCard is the vCard PUT into the address book. RFC 6350 requires CRLF
// line endings, so the lines are joined with \r\n.
var contactVCard = crlf(
	"BEGIN:VCARD",
	"VERSION:3.0",
	"UID:contact-1@go-mailbox-720.test",
	"FN:"+contactFullName,
	"N:Contact;Carol;;;",
	"EMAIL;TYPE=INTERNET:"+contactEmail,
	"END:VCARD",
	"",
)

// seedAddressBook provisions one address-book collection with one vCard via raw
// CardDAV requests: an extended MKCOL then a PUT of the vCard object. Radicale
// does not auto-create the parent collection on a bare PUT, so the MKCOL must
// come first.
func seedAddressBook(t *testing.T, base string) {
	t.Helper()
	bookURL := base + radicaleUser + "/" + addressBookSlug + "/"
	doRadicale(t, "MKCOL", bookURL, "application/xml", mkAddressBookBody, http.StatusCreated)
	doRadicale(t, http.MethodPut, bookURL+"contact-1.vcf", "text/vcard", contactVCard, http.StatusCreated)
}

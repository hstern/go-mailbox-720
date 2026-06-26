package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hstern/go-jscontact"
	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// MeCreateContacts implements POST /me/contacts. It maps the inbound Graph
// contact body onto the neutral contacts.Contact, creates it in the principal's
// default address book, and returns the stored contact (201 Created). The backend
// is obtained via contactsBackend (nil-provider -> 501) and type-asserted to
// contacts.Writer; a read-only backend yields 501.
func (h Handler) MeCreateContacts(ctx context.Context, req *api.MicrosoftGraphContact) (api.MeCreateContactsRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(contacts.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	bookID, ok, err := defaultAddressBookID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No address book to create the contact in; nothing to write against.
		return nil, ht.ErrNotImplemented
	}

	created, err := w.CreateContact(ctx, bookID, graphToContact(req))
	if err != nil {
		return nil, fmt.Errorf("create contact: %w", err)
	}
	return &api.MicrosoftGraphContactStatusCode{
		StatusCode: http.StatusCreated,
		Response:   toGraphContact(created),
	}, nil
}

// MeUpdateContacts implements PATCH /me/contacts/{contact-id}. PATCH is a partial
// update: the current contact is read via GetContact and only the fields present
// in the inbound Graph body overlay it (absent fields are left unchanged), then
// the merged contact — preserving its ID/UID — is written via
// Writer.UpdateContact and returned (200 OK). The backend is obtained via
// contactsBackend (nil-provider -> 501) and type-asserted to contacts.Writer; a
// read-only backend yields 501.
func (h Handler) MeUpdateContacts(ctx context.Context, req *api.MicrosoftGraphContact, params api.MeUpdateContactsParams) (api.MeUpdateContactsRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(contacts.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	current, err := b.GetContact(ctx, params.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	merged := mergeContactPatch(current, req)
	updated, err := h.updateContact(ctx, b, w, merged, params.IfMatch)
	if err != nil {
		return nil, err
	}
	return &api.MicrosoftGraphContactStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphContact(updated),
	}, nil
}

// updateContact writes merged, honouring an inbound If-Match precondition when one
// is supplied and the backend supports conditional writes (CardDAV via a
// conditional PUT). A failed precondition surfaces contacts.ErrPreconditionFailed,
// which the error handler maps to 412. With no If-Match, or a backend that only
// implements the unconditional Writer, it writes unconditionally.
func (h Handler) updateContact(ctx context.Context, b contacts.Backend, w contacts.Writer, merged contacts.Contact, ifMatch api.OptString) (contacts.Contact, error) {
	if etag, conditional := ifMatchOf(ifMatch); conditional {
		if cw, ok := b.(contacts.ConditionalWriter); ok {
			updated, err := cw.UpdateContactIfMatch(ctx, merged, etag)
			if err != nil {
				return contacts.Contact{}, fmt.Errorf("update contact (conditional): %w", err)
			}
			return updated, nil
		}
	}
	updated, err := w.UpdateContact(ctx, merged)
	if err != nil {
		return contacts.Contact{}, fmt.Errorf("update contact: %w", err)
	}
	return updated, nil
}

// MeDeleteContacts implements DELETE /me/contacts/{contact-id}. It type-asserts
// the backend to contacts.Writer (read-only backend -> 501) and deletes the
// contact, returning 204 No Content.
func (h Handler) MeDeleteContacts(ctx context.Context, params api.MeDeleteContactsParams) (api.MeDeleteContactsRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	w, ok := b.(contacts.Writer)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	if err := w.DeleteContact(ctx, params.ContactID); err != nil {
		return nil, fmt.Errorf("delete contact: %w", err)
	}
	return &api.MeDeleteContactsNoContent{}, nil
}

// graphToContact maps the inbound Graph contact onto the neutral
// contacts.Contact — the inverse of toGraphContact. Read-only and
// server-assigned fields (ID, UID) are ignored: the backend stamps the created
// contact with its own opaque ID/UID. The flat Graph DTO is projected onto the
// rich jscontact.Card via the Card builders (SetName/SetOrganization/…).
func graphToContact(gc *api.MicrosoftGraphContact) contacts.Contact {
	var c contacts.Contact
	c.SetName(gc.DisplayName.Or(""), gc.GivenName.Or(""), gc.Surname.Or(""))
	c.SetOrganization(gc.CompanyName.Or(""))
	c.SetTitle(gc.JobTitle.Or(""))
	c.SetNote(gc.PersonalNotes.Or(""))
	c.SetEmails(graphToEmails(gc.EmailAddresses))
	c.SetPhones(graphToPhones(gc))
	return c
}

// mergeContactPatch overlays the fields present in the inbound Graph PATCH body
// onto the current contact, leaving absent fields unchanged — the
// read-modify-write half of PATCH semantics. Presence is detected per field:
// scalar OptNil fields via .Get() (a set field overlays, even when its value is
// empty), the EmailAddresses collection via a non-empty slice, and the phones
// per-kind (see mergePhones). The contact's identity (ID, UID, and the rest of
// the current record's embedded Card) is preserved: the merge mutates the
// projected fields of a copy of current via the Card setters, so UpdateContact
// rewrites in place.
//
// The projected fields (DisplayName/Organization/…) are read-only methods, not
// addressable struct fields, so each overlay routes through the corresponding
// Card setter. Name is reconciled as a unit (SetName takes display+given+surname
// together) from current's values plus any present patch fields.
func mergeContactPatch(current contacts.Contact, gc *api.MicrosoftGraphContact) contacts.Contact {
	merged := current

	// Name: overlay only the present components onto current's, then rebuild the
	// Card Name as a unit (SetName clears+rewrites display/given/surname).
	display, given, surname := current.DisplayName(), current.GivenName(), current.Surname()
	nameTouched := false
	if v, ok := gc.DisplayName.Get(); ok {
		display, nameTouched = v, true
	}
	if v, ok := gc.GivenName.Get(); ok {
		given, nameTouched = v, true
	}
	if v, ok := gc.Surname.Get(); ok {
		surname, nameTouched = v, true
	}
	if nameTouched {
		merged.SetName(display, given, surname)
	}

	if v, ok := gc.CompanyName.Get(); ok {
		setOrClearOrganization(&merged, v)
	}
	if v, ok := gc.JobTitle.Get(); ok {
		setOrClearTitle(&merged, v)
	}
	if v, ok := gc.PersonalNotes.Get(); ok {
		setOrClearNote(&merged, v)
	}
	if len(gc.EmailAddresses) > 0 {
		merged.SetEmails(graphToEmails(gc.EmailAddresses))
	}
	if len(gc.BusinessPhones) > 0 || len(gc.HomePhones) > 0 || gc.MobilePhone.Set {
		merged.SetPhones(mergePhones(current.PhoneList(), gc))
	}
	return merged
}

// setOrClearOrganization overlays a present Graph companyName: a non-empty value
// sets the single organization, an empty value clears it (SetOrganization is a
// no-op on empty, so the clear is explicit here to honour a present-but-empty
// PATCH field).
func setOrClearOrganization(c *contacts.Contact, name string) {
	if name == "" {
		c.Organizations = nil
		return
	}
	c.SetOrganization(name)
}

// setOrClearTitle mirrors setOrClearOrganization for the single job title.
func setOrClearTitle(c *contacts.Contact, name string) {
	if name == "" {
		c.Titles = nil
		return
	}
	c.SetTitle(name)
}

// setOrClearNote mirrors setOrClearOrganization for the single note.
func setOrClearNote(c *contacts.Contact, note string) {
	if note == "" {
		c.Notes = nil
		return
	}
	c.SetNote(note)
}

// mergePhones overlays the inbound Graph phone fields onto the current phones
// per-kind. Microsoft Graph exposes phones as three separate properties
// (businessPhones/homePhones/mobilePhone) that this server collapses into one
// neutral phone list, so a partial PATCH must replace only the kinds it carries:
// patching just mobilePhone must not drop the existing work/home numbers. A
// present-but-empty mobilePhone clears the mobile number; absent kinds (and any
// non-standard phone types) are carried over from current.
func mergePhones(current []jscontact.Phone, gc *api.MicrosoftGraphContact) []jscontact.Phone {
	work, home, cell := []jscontact.Phone(nil), []jscontact.Phone(nil), []jscontact.Phone(nil)
	var other []jscontact.Phone
	for _, p := range current {
		switch contacts.PhoneType(p) {
		case "work":
			work = append(work, p)
		case "home":
			home = append(home, p)
		case "cell":
			cell = append(cell, p)
		default:
			other = append(other, p)
		}
	}

	if len(gc.BusinessPhones) > 0 {
		work = numbersOfType(gc.BusinessPhones, "work")
	}
	if len(gc.HomePhones) > 0 {
		home = numbersOfType(gc.HomePhones, "home")
	}
	if v, ok := gc.MobilePhone.Get(); ok {
		if strings.TrimSpace(v) == "" {
			cell = nil // explicit clear of the mobile number
		} else {
			cell = []jscontact.Phone{contacts.NewPhone(v, "cell")}
		}
	}

	out := make([]jscontact.Phone, 0, len(work)+len(home)+len(cell)+len(other))
	out = append(out, work...)
	out = append(out, home...)
	out = append(out, cell...)
	out = append(out, other...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// numbersOfType maps a Graph phone-number list onto neutral phones of the given
// JSContact type, skipping blank entries.
func numbersOfType(nums []api.NilString, typ string) []jscontact.Phone {
	var out []jscontact.Phone
	for _, p := range nums {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.NewPhone(v, typ))
		}
	}
	return out
}

// graphToEmails maps Graph emailAddress objects onto neutral jscontact email
// values — the inverse of toGraphEmailAddresses (the Graph name carries the
// JSContact type label).
func graphToEmails(emails []api.MicrosoftGraphEmailAddress) []jscontact.EmailAddress {
	if len(emails) == 0 {
		return nil
	}
	out := make([]jscontact.EmailAddress, 0, len(emails))
	for _, e := range emails {
		out = append(out, contacts.NewEmail(e.Address.Or(""), e.Name.Or("")))
	}
	return out
}

// graphToPhones gathers the Graph contact's type-specific phone fields back into
// neutral jscontact phone values — the inverse of addPhones. The destination
// field dictates the JSContact type: mobilePhone -> "cell", homePhones ->
// "home", businessPhones -> "work".
func graphToPhones(gc *api.MicrosoftGraphContact) []jscontact.Phone {
	var out []jscontact.Phone
	for _, p := range gc.BusinessPhones {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.NewPhone(v, "work"))
		}
	}
	for _, p := range gc.HomePhones {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.NewPhone(v, "home"))
		}
	}
	if v, ok := gc.MobilePhone.Get(); ok && strings.TrimSpace(v) != "" {
		out = append(out, contacts.NewPhone(v, "cell"))
	}
	return out
}

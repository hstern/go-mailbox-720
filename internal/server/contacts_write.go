package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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

	updated, err := w.UpdateContact(ctx, mergeContactPatch(current, req))
	if err != nil {
		return nil, fmt.Errorf("update contact: %w", err)
	}
	return &api.MicrosoftGraphContactStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphContact(updated),
	}, nil
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
// contact with its own opaque ID/UID.
func graphToContact(gc *api.MicrosoftGraphContact) contacts.Contact {
	return contacts.Contact{
		DisplayName:  gc.DisplayName.Or(""),
		GivenName:    gc.GivenName.Or(""),
		Surname:      gc.Surname.Or(""),
		Organization: gc.CompanyName.Or(""),
		Title:        gc.JobTitle.Or(""),
		Note:         gc.PersonalNotes.Or(""),
		Emails:       graphToEmails(gc.EmailAddresses),
		Phones:       graphToPhones(gc),
	}
}

// mergeContactPatch overlays the fields present in the inbound Graph PATCH body
// onto the current contact, leaving absent fields unchanged — the
// read-modify-write half of PATCH semantics. Presence is detected per field:
// scalar OptNil fields via .Get() (a set field overlays, even when its value is
// empty), the EmailAddresses collection via a non-empty slice, and the phones
// per-kind (see mergePhones). The contact's identity (ID, UID, and the rest of
// the current record) is preserved so UpdateContact rewrites in place.
func mergeContactPatch(current contacts.Contact, gc *api.MicrosoftGraphContact) contacts.Contact {
	merged := current
	if v, ok := gc.DisplayName.Get(); ok {
		merged.DisplayName = v
	}
	if v, ok := gc.GivenName.Get(); ok {
		merged.GivenName = v
	}
	if v, ok := gc.Surname.Get(); ok {
		merged.Surname = v
	}
	if v, ok := gc.CompanyName.Get(); ok {
		merged.Organization = v
	}
	if v, ok := gc.JobTitle.Get(); ok {
		merged.Title = v
	}
	if v, ok := gc.PersonalNotes.Get(); ok {
		merged.Note = v
	}
	if len(gc.EmailAddresses) > 0 {
		merged.Emails = graphToEmails(gc.EmailAddresses)
	}
	if len(gc.BusinessPhones) > 0 || len(gc.HomePhones) > 0 || gc.MobilePhone.Set {
		merged.Phones = mergePhones(current.Phones, gc)
	}
	return merged
}

// mergePhones overlays the inbound Graph phone fields onto the current phones
// per-kind. Microsoft Graph exposes phones as three separate properties
// (businessPhones/homePhones/mobilePhone) that this server collapses into one
// neutral []Phone, so a partial PATCH must replace only the kinds it carries:
// patching just mobilePhone must not drop the existing work/home numbers. A
// present-but-empty mobilePhone clears the mobile number; absent kinds (and any
// non-standard phone types) are carried over from current.
func mergePhones(current []contacts.Phone, gc *api.MicrosoftGraphContact) []contacts.Phone {
	work, home, cell := []contacts.Phone(nil), []contacts.Phone(nil), []contacts.Phone(nil)
	var other []contacts.Phone
	for _, p := range current {
		switch p.Type {
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
			cell = []contacts.Phone{{Number: v, Type: "cell"}}
		}
	}

	out := make([]contacts.Phone, 0, len(work)+len(home)+len(cell)+len(other))
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
// vCard TYPE, skipping blank entries.
func numbersOfType(nums []api.NilString, typ string) []contacts.Phone {
	var out []contacts.Phone
	for _, p := range nums {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.Phone{Number: v, Type: typ})
		}
	}
	return out
}

// graphToEmails maps Graph emailAddress objects onto neutral contacts.EmailAddress
// values — the inverse of toGraphEmailAddresses (the Graph name carries the vCard
// TYPE label).
func graphToEmails(emails []api.MicrosoftGraphEmailAddress) []contacts.EmailAddress {
	if len(emails) == 0 {
		return nil
	}
	out := make([]contacts.EmailAddress, 0, len(emails))
	for _, e := range emails {
		out = append(out, contacts.EmailAddress{
			Address: e.Address.Or(""),
			Type:    e.Name.Or(""),
		})
	}
	return out
}

// graphToPhones gathers the Graph contact's type-specific phone fields back into
// neutral contacts.Phone values — the inverse of addPhones. The destination field
// dictates the vCard TYPE: mobilePhone -> "cell", homePhones -> "home",
// businessPhones -> "work".
func graphToPhones(gc *api.MicrosoftGraphContact) []contacts.Phone {
	var out []contacts.Phone
	for _, p := range gc.BusinessPhones {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.Phone{Number: v, Type: "work"})
		}
	}
	for _, p := range gc.HomePhones {
		if v, ok := p.Get(); ok && strings.TrimSpace(v) != "" {
			out = append(out, contacts.Phone{Number: v, Type: "home"})
		}
	}
	if v, ok := gc.MobilePhone.Get(); ok && strings.TrimSpace(v) != "" {
		out = append(out, contacts.Phone{Number: v, Type: "cell"})
	}
	return out
}

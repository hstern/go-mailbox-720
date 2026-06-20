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
//
// Deferred: MeUpdateContacts (PATCH) needs a read-modify-write merge of the
// partial body onto the stored contact and is left for a follow-up.
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

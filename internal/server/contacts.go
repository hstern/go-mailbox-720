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

// ContactsProvider yields a contacts.Backend for an authenticated request. The
// static implementation lives in cmd/mailboxd; per-identity providers (mapping
// the token's mailbox identity to backend credentials) come later. It mirrors
// MailProvider and CalendarProvider.
type ContactsProvider interface {
	Contacts(ctx context.Context) (contacts.Backend, error)
}

// contactsBackend resolves the request's contacts backend, or reports "not
// implemented" when no provider is configured (the skeleton posture). Mirrors
// Handler.backend for the mail port and Handler.calendarBackend.
func (h Handler) contactsBackend(ctx context.Context) (contacts.Backend, error) {
	if h.contacts == nil {
		return nil, ht.ErrNotImplemented
	}
	return h.contacts.Contacts(ctx)
}

// MeListContacts implements GET /me/contacts by listing the contacts of the
// principal's default address book. The Graph /me/contacts collection is the
// user's default contacts folder; the CardDAV port addresses contacts by
// address book, so this resolves the first (default) address book and lists its
// contacts — analogous to /me/events defaulting to the primary calendar.
func (h Handler) MeListContacts(ctx context.Context, params api.MeListContactsParams) (api.MeListContactsRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	bookID, ok, err := defaultAddressBookID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No address books for this principal — no contacts, rather than querying
		// with an empty (invalid) address-book id.
		return &api.MicrosoftGraphContactCollectionResponseStatusCode{
			StatusCode: http.StatusOK,
			Response:   api.MicrosoftGraphContactCollectionResponse{Value: []api.MicrosoftGraphContact{}},
		}, nil
	}

	cs, err := b.ListContacts(ctx, bookID)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	value := make([]api.MicrosoftGraphContact, 0, len(cs))
	for _, c := range cs {
		value = append(value, toGraphContact(c))
	}
	return &api.MicrosoftGraphContactCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphContactCollectionResponse{Value: value},
	}, nil
}

// MeGetContacts implements GET /me/contacts/{contact-id}.
func (h Handler) MeGetContacts(ctx context.Context, params api.MeGetContactsParams) (api.MeGetContactsRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	c, err := b.GetContact(ctx, params.ContactID)
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}
	return &api.MicrosoftGraphContactStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphContact(c),
	}, nil
}

// toGraphContact maps the neutral contacts.Contact onto the generated Graph type.
// The rich jscontact.Card is projected onto the flat, single-valued Graph DTO via
// the helper methods (DisplayName/GivenName/… read the embedded Card).
func toGraphContact(c contacts.Contact) api.MicrosoftGraphContact {
	gc := api.MicrosoftGraphContact{
		ID:             api.NewOptString(c.ID),
		DisplayName:    api.NewOptNilString(c.DisplayName()),
		GivenName:      api.NewOptNilString(c.GivenName()),
		Surname:        api.NewOptNilString(c.Surname()),
		CompanyName:    api.NewOptNilString(c.Organization()),
		JobTitle:       api.NewOptNilString(c.Title()),
		EmailAddresses: toGraphEmailAddresses(c.EmailList()),
	}
	if c.Note() != "" {
		gc.PersonalNotes = api.NewOptNilString(c.Note())
	}
	addPhones(&gc, c.PhoneList())
	if c.ETag != "" {
		gc.OdataDotEtag = api.NewOptString(c.ETag)
	}
	return gc
}

// toGraphEmailAddresses maps the contact's emails onto Graph emailAddress
// objects. The JSContact type label (contacts.EmailType) becomes the Graph name,
// the closest analog the emailAddress shape offers.
func toGraphEmailAddresses(emails []jscontact.EmailAddress) []api.MicrosoftGraphEmailAddress {
	if len(emails) == 0 {
		return nil
	}
	out := make([]api.MicrosoftGraphEmailAddress, 0, len(emails))
	for _, e := range emails {
		out = append(out, api.MicrosoftGraphEmailAddress{
			Address: api.NewOptNilString(e.Address),
			Name:    api.NewOptNilString(contacts.EmailType(e)),
		})
	}
	return out
}

// addPhones distributes the contact's phone numbers across the Graph contact's
// type-specific phone fields. Graph splits phone numbers by kind (business,
// home, mobile) rather than carrying a TYPE label, so the JSContact phone type
// (contacts.PhoneType) selects the destination: "cell"/"mobile" -> mobilePhone (a
// single value, first wins), "home" -> homePhones, everything else ->
// businessPhones (the Graph default).
func addPhones(gc *api.MicrosoftGraphContact, phones []jscontact.Phone) {
	for _, p := range phones {
		switch t := strings.ToLower(contacts.PhoneType(p)); {
		case strings.Contains(t, "cell"), strings.Contains(t, "mobile"):
			if !gc.MobilePhone.Set {
				gc.MobilePhone = api.NewOptNilString(p.Number)
			}
		case strings.Contains(t, "home"):
			gc.HomePhones = append(gc.HomePhones, api.NewNilString(p.Number))
		default:
			gc.BusinessPhones = append(gc.BusinessPhones, api.NewNilString(p.Number))
		}
	}
}

// defaultAddressBookID resolves the principal's default address book — the first
// address book the backend reports. The CardDAV port has no "default folder"
// shortcut, so /me/contacts resolves it explicitly. ok is false when the
// principal has no address books; the caller must not query with the empty id
// (it is not a valid address book). Mirrors defaultCalendarID.
func defaultAddressBookID(ctx context.Context, b contacts.Backend) (id string, ok bool, err error) {
	books, err := b.ListAddressBooks(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list address books: %w", err)
	}
	if len(books) == 0 {
		return "", false, nil
	}
	return books[0].ID, true, nil
}

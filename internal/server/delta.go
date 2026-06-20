package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// MeMessagesDelta implements GET /me/messages/delta — incremental sync of the
// inbox. It type-asserts the mail backend to mail.DeltaReader (a capability the
// IMAP adapter provides; a backend without it yields 501) and asks for the
// messages changed since the opaque continuation token the client echoed back
// from a prior @odata.deltaLink. The returned next token is folded into this
// response's @odata.deltaLink, which the client follows for the next round.
//
// This first cut is additive (new messages by UID); reporting deletions and
// flag/read-state changes needs IMAP CONDSTORE/QRESYNC and is future work.
func (h Handler) MeMessagesDelta(ctx context.Context, params api.MeMessagesDeltaParams) (api.MeMessagesDeltaRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(mail.DeltaReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	// $deltatoken carries our opaque sync token for the next round; $skiptoken is
	// the paging variant. Either (or neither, for the initial sync) is accepted.
	token := params.Deltatoken.Or(params.Skiptoken.Or(""))

	msgs, next, err := d.Delta(ctx, "", token)
	if err != nil {
		// A bad continuation token is client error: tell the client to resync
		// (drop the token and start over) rather than 500 a request that can
		// never succeed.
		if errors.Is(err, mail.ErrInvalidDeltaToken) {
			return resyncRequired(), nil
		}
		return nil, fmt.Errorf("messages delta: %w", err)
	}

	value := make([]api.MicrosoftGraphMessage, 0, len(msgs))
	for _, m := range msgs {
		value = append(value, toGraphMessage(m))
	}
	return &api.MeMessagesDelta2XXStatusCode{
		StatusCode: http.StatusOK,
		Response: api.MeMessagesDelta2XX{
			Value:             value,
			OdataDotDeltaLink: api.NewOptNilString(deltaLink("/me/messages/delta()", next)),
		},
	}, nil
}

// MeEventsDelta implements GET /me/events/delta — incremental sync of the
// principal's primary calendar via the CalDAV adapter's calendar.DeltaReader
// (RFC 6578 sync-token), folding the next token into @odata.deltaLink. A backend
// without delta support yields 501; a principal with no calendar yields an empty
// delta. Additive first cut (created/updated events; deletions are future work).
func (h Handler) MeEventsDelta(ctx context.Context, params api.MeEventsDeltaParams) (api.MeEventsDeltaRes, error) {
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(calendar.DeltaReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	calID, ok, err := defaultCalendarID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &api.MeEventsDelta2XXStatusCode{
			StatusCode: http.StatusOK,
			Response:   api.MeEventsDelta2XX{Value: []api.MicrosoftGraphEvent{}},
		}, nil
	}

	events, next, err := d.Delta(ctx, calID, params.Deltatoken.Or(params.Skiptoken.Or("")))
	if err != nil {
		return nil, fmt.Errorf("events delta: %w", err)
	}
	value := make([]api.MicrosoftGraphEvent, 0, len(events))
	for _, e := range events {
		value = append(value, toGraphEvent(e))
	}
	return &api.MeEventsDelta2XXStatusCode{
		StatusCode: http.StatusOK,
		Response: api.MeEventsDelta2XX{
			Value:             value,
			OdataDotDeltaLink: api.NewOptNilString(deltaLink("/me/events/delta()", next)),
		},
	}, nil
}

// MeContactsDelta implements GET /me/contacts/delta — incremental sync of the
// principal's default address book via the CardDAV adapter's contacts.DeltaReader
// (RFC 6578 sync-token). Same shape as MeEventsDelta.
func (h Handler) MeContactsDelta(ctx context.Context, params api.MeContactsDeltaParams) (api.MeContactsDeltaRes, error) {
	b, err := h.contactsBackend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(contacts.DeltaReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}

	bookID, ok, err := defaultAddressBookID(ctx, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &api.MeContactsDelta2XXStatusCode{
			StatusCode: http.StatusOK,
			Response:   api.MeContactsDelta2XX{Value: []api.MicrosoftGraphContact{}},
		}, nil
	}

	changed, next, err := d.Delta(ctx, bookID, params.Deltatoken.Or(params.Skiptoken.Or("")))
	if err != nil {
		return nil, fmt.Errorf("contacts delta: %w", err)
	}
	value := make([]api.MicrosoftGraphContact, 0, len(changed))
	for _, c := range changed {
		value = append(value, toGraphContact(c))
	}
	return &api.MeContactsDelta2XXStatusCode{
		StatusCode: http.StatusOK,
		Response: api.MeContactsDelta2XX{
			Value:             value,
			OdataDotDeltaLink: api.NewOptNilString(deltaLink("/me/contacts/delta()", next)),
		},
	}, nil
}

// deltaLink builds the @odata.deltaLink a client GETs for the next sync round:
// the given delta operation path under the API base prefix, carrying the opaque
// continuation token.
func deltaLink(opPath, token string) string {
	return basePath + opPath + "?$deltatoken=" + url.QueryEscape(token)
}

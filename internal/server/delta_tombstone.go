package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-faster/jx"
	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/grapherr"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// MessagesDeltaHandler serves GET /me/messages/delta with @removed tombstones for
// expunged messages. Like the events/contacts delta handlers it is a custom
// http.Handler (not a generated operation) because the value array mixes full
// messages with tombstones; it reuses the generated per-message encoder for the
// changed messages. The IMAP adapter reports deletions via QRESYNC VANISHED.
func MessagesDeltaHandler(p MailProvider) http.Handler {
	return http.HandlerFunc((Handler{mail: p}).serveMessagesDelta)
}

func (h Handler) serveMessagesDelta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := h.backend(ctx)
	if err != nil {
		grapherr.Write(w, statusFor(err))
		return
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(mail.DeltaReader)
	if !ok {
		grapherr.Write(w, http.StatusNotImplemented)
		return
	}

	token := r.URL.Query().Get("$deltatoken")
	if token == "" {
		token = r.URL.Query().Get("$skiptoken")
	}
	changed, removed, next, err := d.Delta(ctx, "", token)
	if err != nil {
		switch {
		case errors.Is(err, mail.ErrInvalidDeltaToken):
			// Tell the client to drop the token and resync rather than fail forever.
			grapherr.Write(w, http.StatusGone)
		case errors.Is(err, mail.ErrDeltaUnsupported):
			// The IMAP server lacks QRESYNC, so delta cannot be served.
			grapherr.Write(w, http.StatusNotImplemented)
		default:
			grapherr.Write(w, http.StatusInternalServerError)
		}
		return
	}

	writeDelta(w, deltaLink("/me/messages/delta()", next), removed, func(e *jx.Encoder) {
		for _, m := range changed {
			gm := toGraphMessage(m)
			gm.Encode(e)
		}
	})
}

// deltaLink builds the @odata.deltaLink a client GETs for the next sync round:
// the given delta operation path under the API base prefix, carrying the opaque
// continuation token.
func deltaLink(opPath, token string) string {
	return basePath + opPath + "?$deltatoken=" + url.QueryEscape(token)
}

// EventsDeltaHandler and ContactsDeltaHandler serve GET /me/events/delta and
// /me/contacts/delta with @removed tombstones for deleted items. They are plain
// http.Handlers (not generated ogen operations) because a delta response's value
// array mixes full objects with tombstones —
//
//	{"id":"…","@removed":{"reason":"deleted"}}
//
// — which the generated typed collection (a []MicrosoftGraphEvent) cannot carry.
// They reuse the generated per-item encoder for changed objects, so the object
// JSON stays identical to the read/$batch paths, and hand-write the tombstones.
// mailboxd mounts these ahead of the generated server for the two delta paths.
//
// A nil/unsupported backend yields the same Graph 501 the generated handlers do.
// Deletions are reported by the CalDAV/CardDAV sync-collection; the mail delta is
// additive (no deletions without CONDSTORE) and keeps its generated handler.

// EventsDeltaHandler serves GET /me/events/delta with tombstones.
func EventsDeltaHandler(p CalendarProvider) http.Handler {
	return http.HandlerFunc((Handler{calendar: p}).serveEventsDelta)
}

func (h Handler) serveEventsDelta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := h.calendarBackend(ctx)
	if err != nil {
		grapherr.Write(w, statusFor(err))
		return
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(calendar.DeltaReader)
	if !ok {
		grapherr.Write(w, http.StatusNotImplemented)
		return
	}
	calID, ok, err := defaultCalendarID(ctx, b)
	if err != nil {
		grapherr.Write(w, http.StatusInternalServerError)
		return
	}

	var changed []calendar.Event
	var removed []string
	var next string
	if ok {
		if changed, removed, next, err = d.Delta(ctx, calID, r.URL.Query().Get("$deltatoken")); err != nil {
			grapherr.Write(w, http.StatusInternalServerError)
			return
		}
	}

	writeDelta(w, deltaLink("/me/events/delta()", next), removed, func(e *jx.Encoder) {
		for _, ev := range changed {
			ge := toGraphEvent(ev)
			ge.Encode(e)
		}
	})
}

// ContactsDeltaHandler serves GET /me/contacts/delta with tombstones.
func ContactsDeltaHandler(p ContactsProvider) http.Handler {
	return http.HandlerFunc((Handler{contacts: p}).serveContactsDelta)
}

func (h Handler) serveContactsDelta(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := h.contactsBackend(ctx)
	if err != nil {
		grapherr.Write(w, statusFor(err))
		return
	}
	defer func() { _ = b.Close() }()

	d, ok := b.(contacts.DeltaReader)
	if !ok {
		grapherr.Write(w, http.StatusNotImplemented)
		return
	}
	bookID, ok, err := defaultAddressBookID(ctx, b)
	if err != nil {
		grapherr.Write(w, http.StatusInternalServerError)
		return
	}

	var changed []contacts.Contact
	var removed []string
	var next string
	if ok {
		if changed, removed, next, err = d.Delta(ctx, bookID, r.URL.Query().Get("$deltatoken")); err != nil {
			grapherr.Write(w, http.StatusInternalServerError)
			return
		}
	}

	writeDelta(w, deltaLink("/me/contacts/delta()", next), removed, func(e *jx.Encoder) {
		for _, c := range changed {
			gc := toGraphContact(c)
			gc.Encode(e)
		}
	})
}

// writeDelta serializes a Graph delta page: a JSON object with @odata.deltaLink
// and a value array of changed objects (written by encodeChanged) followed by an
// @removed tombstone per deleted id.
func writeDelta(w http.ResponseWriter, deltaLink string, removed []string, encodeChanged func(*jx.Encoder)) {
	var e jx.Encoder
	e.ObjStart()
	e.FieldStart("@odata.deltaLink")
	e.Str(deltaLink)
	e.FieldStart("value")
	e.ArrStart()
	encodeChanged(&e)
	for _, id := range removed {
		e.ObjStart()
		e.FieldStart("id")
		e.Str(id)
		e.FieldStart("@removed")
		e.ObjStart()
		e.FieldStart("reason")
		e.Str("deleted")
		e.ObjEnd()
		e.ObjEnd()
	}
	e.ArrEnd()
	e.ObjEnd()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(e.Bytes())
}

// statusFor maps a backend-acquisition error to a Graph status: the
// not-implemented sentinel (no provider configured) becomes 501, anything else a
// 500.
func statusFor(err error) int {
	if errors.Is(err, ht.ErrNotImplemented) {
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}

package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/grapherr"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/ogen-go/ogen/ogenerrors"
)

// graphErrorHandler renders any handler or request-decoding error as a Graph
// error object. A failed If-Match precondition from any backing-store port maps
// to 412 Precondition Failed; otherwise the status decision is deferred to
// ogenerrors.ErrorCode, which maps ht.ErrNotImplemented -> 501, decode errors ->
// 400, bad content type -> 415, and everything else -> 500.
func graphErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, mail.ErrPreconditionFailed) ||
		errors.Is(err, calendar.ErrPreconditionFailed) ||
		errors.Is(err, contacts.ErrPreconditionFailed) {
		grapherr.Write(w, http.StatusPreconditionFailed)
		return
	}
	grapherr.Write(w, ogenerrors.ErrorCode(err))
}

// graphNotFoundHandler renders a Graph 404 for requests that match no route.
func graphNotFoundHandler(w http.ResponseWriter, _ *http.Request) {
	grapherr.Write(w, http.StatusNotFound)
}

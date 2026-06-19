package server

import (
	"context"
	"net/http"

	"github.com/hstern/go-mailbox-720/internal/grapherr"
	"github.com/ogen-go/ogen/ogenerrors"
)

// graphErrorHandler renders any handler or request-decoding error as a Graph
// error object. It defers the status decision to ogenerrors.ErrorCode, which
// maps ht.ErrNotImplemented -> 501, decode errors -> 400, bad content type ->
// 415, and everything else -> 500.
func graphErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	grapherr.Write(w, ogenerrors.ErrorCode(err))
}

// graphNotFoundHandler renders a Graph 404 for requests that match no route.
func graphNotFoundHandler(w http.ResponseWriter, _ *http.Request) {
	grapherr.Write(w, http.StatusNotFound)
}

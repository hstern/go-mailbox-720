package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ogen-go/ogen/ogenerrors"
)

// graphError is the Microsoft Graph error-response envelope:
//
//	{ "error": { "code": "...", "message": "..." } }
//
// Graph clients branch on the machine-readable code; the message is human-facing.
type graphError struct {
	Error graphErrorBody `json:"error"`
}

type graphErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type graphStatusMeta struct {
	code    string
	message string
}

// graphStatusTable maps an HTTP status to its Graph error code + generic message.
// Statuses absent here fall back to 500/generalException (see writeGraphError).
var graphStatusTable = map[int]graphStatusMeta{
	http.StatusBadRequest:           {"badRequest", "The request is malformed or incorrect."},
	http.StatusUnauthorized:         {"unauthenticated", "Authentication is required."},
	http.StatusForbidden:            {"forbidden", "Access is denied."},
	http.StatusNotFound:             {"notFound", "The requested resource does not exist."},
	http.StatusMethodNotAllowed:     {"methodNotAllowed", "The HTTP method is not allowed."},
	http.StatusUnsupportedMediaType: {"unsupportedMediaType", "The content type is not supported."},
	http.StatusNotImplemented:       {"notImplemented", "This operation is not implemented."},
}

// graphErrorHandler renders any handler or request-decoding error as a Graph
// error object. It defers the status decision to ogenerrors.ErrorCode, which
// maps ht.ErrNotImplemented -> 501, decode errors -> 400, bad content type ->
// 415, and everything else -> 500.
func graphErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	writeGraphError(w, ogenerrors.ErrorCode(err))
}

// graphNotFoundHandler renders a Graph 404 for requests that match no route.
func graphNotFoundHandler(w http.ResponseWriter, _ *http.Request) {
	writeGraphError(w, http.StatusNotFound)
}

func writeGraphError(w http.ResponseWriter, status int) {
	meta, ok := graphStatusTable[status]
	if !ok {
		status = http.StatusInternalServerError
		meta = graphStatusMeta{"generalException", "An unexpected error occurred."}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(graphError{Error: graphErrorBody{Code: meta.code, Message: meta.message}})
}

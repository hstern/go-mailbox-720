// Package grapherr renders the Microsoft Graph error-object response shape,
// shared by the HTTP server and the auth middleware so every error looks the
// same on the wire:
//
//	{ "error": { "code": "...", "message": "..." } }
//
// Graph clients branch on the machine-readable code; the message is human-facing.
package grapherr

import (
	"encoding/json"
	"net/http"
)

type object struct {
	Error body `json:"error"`
}

type body struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type meta struct {
	code    string
	message string
}

// table maps an HTTP status to its Graph error code + generic message. Statuses
// absent here fall back to 500/generalException (see Write).
var table = map[int]meta{
	http.StatusBadRequest:           {"badRequest", "The request is malformed or incorrect."},
	http.StatusUnauthorized:         {"unauthenticated", "Authentication is required."},
	http.StatusForbidden:            {"forbidden", "Access is denied."},
	http.StatusNotFound:             {"notFound", "The requested resource does not exist."},
	http.StatusGone:                 {"resyncRequired", "The delta token is invalid or expired; restart with an initial sync."},
	http.StatusMethodNotAllowed:     {"methodNotAllowed", "The HTTP method is not allowed."},
	http.StatusUnsupportedMediaType: {"unsupportedMediaType", "The content type is not supported."},
	http.StatusNotImplemented:       {"notImplemented", "This operation is not implemented."},
}

// Write renders status as a Graph error object. An unmapped status is reported
// as 500/generalException.
func Write(w http.ResponseWriter, status int) {
	m, ok := table[status]
	if !ok {
		status = http.StatusInternalServerError
		m = meta{"generalException", "An unexpected error occurred."}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(object{Error: body{Code: m.code, Message: m.message}})
}

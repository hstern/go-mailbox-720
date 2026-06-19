// Package server implements the Microsoft Graph mailbox HTTP server.
//
// Handler embeds the ogen-generated api.UnimplementedHandler, so every operation
// returns "not implemented" until a later issue fills it in. New wires the ogen
// server with the Graph base path (/v1.0), the Graph error-object response shape,
// and a Graph-shaped 404 for unrouted requests.
//
// Success responses use the OData envelope already modeled by the generated
// collection types (@odata.context / value / @odata.nextLink); this skeleton
// establishes only the cross-cutting error-object shape (see errors.go) — the
// per-operation success envelopes are filled in alongside OData query execution
// (MB720-6+).
//
// This package imports the generated api package (internal/graph/api), which is
// git-excluded; run `go generate ./internal/graph` before building.
package server

import "github.com/hstern/go-mailbox-720/internal/graph/api"

// basePath is the Graph API version segment. The conformance harness points the
// SDK base URL at http://localhost:8080/v1.0 (see HANDOFF.md "Testing"), so the
// generated router is mounted under this prefix.
const basePath = "/v1.0"

// Handler is the mailbox server's Graph handler. Embedding UnimplementedHandler
// satisfies the full generated api.Handler interface; operations are implemented
// incrementally by defining methods on Handler in later issues.
type Handler struct {
	api.UnimplementedHandler
}

// New builds the mailbox server's HTTP handler (an *api.Server, which is an
// http.Handler).
func New() (*api.Server, error) {
	return api.NewServer(Handler{},
		api.WithPathPrefix(basePath),
		api.WithErrorHandler(graphErrorHandler),
		api.WithNotFound(graphNotFoundHandler),
	)
}

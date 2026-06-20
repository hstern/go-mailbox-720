package auth

import (
	"encoding/json"
	"net/http"
)

// WellKnownProtectedResource is the RFC 9728 §3.1 metadata path.
const WellKnownProtectedResource = "/.well-known/oauth-protected-resource"

// ProtectedResourceMetadata is the subset of the RFC 9728 OAuth 2.0 Protected
// Resource Metadata document this server publishes about itself: the resource
// identifier, the authorization servers whose tokens it accepts, the scopes it
// enforces, and how it expects the token (the Authorization header only).
//
// INTERIM: this minimal server-side document migrates to the
// go-protected-resource-metadata library once it ships (MB720-17); a resource
// server only needs to serve a static document, so the full typed model + client +
// validation in that library are not duplicated here.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// MetadataHandler serves this resource's RFC 9728 metadata. It is PUBLIC — clients
// fetch it unauthenticated to discover how to obtain a usable token — so it must be
// mounted OUTSIDE the auth middleware. The document is derived from cfg: ResourceID
// is the required "resource" member, Issuers are the authorization servers, and
// RequiredScopes are advertised as scopes_supported.
func MetadataHandler(cfg Config) http.Handler {
	doc, _ := json.Marshal(ProtectedResourceMetadata{
		Resource:               cfg.ResourceID,
		AuthorizationServers:   cfg.Issuers,
		ScopesSupported:        cfg.RequiredScopes,
		BearerMethodsSupported: []string{"header"}, // RFC 6750 §2.1 only (not form/query)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
}

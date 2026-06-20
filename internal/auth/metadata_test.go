package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	prm "github.com/hstern/go-protected-resource-metadata"
)

func TestResourceMetadata(t *testing.T) {
	m := ResourceMetadata(Config{
		ResourceID:     "https://mailbox.example.com",
		Issuers:        []string{"https://idp.example.com"},
		RequiredScopes: []string{"Mail.Read"},
	})
	if m.Resource != "https://mailbox.example.com" {
		t.Errorf("resource = %q", m.Resource)
	}
	if len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != "https://idp.example.com" {
		t.Errorf("authorization_servers = %v", m.AuthorizationServers)
	}
	if len(m.ScopesSupported) != 1 || m.ScopesSupported[0] != "Mail.Read" {
		t.Errorf("scopes_supported = %v", m.ScopesSupported)
	}
	if len(m.BearerMethodsSupported) != 1 || m.BearerMethodsSupported[0] != prm.BearerMethodHeader {
		t.Errorf("bearer_methods_supported = %v", m.BearerMethodsSupported)
	}
}

func TestMetadataEndpoint(t *testing.T) {
	path, h, err := MetadataEndpoint(Config{
		ResourceID:     "https://mailbox.example.com",
		Issuers:        []string{"https://idp.example.com"},
		RequiredScopes: []string{"Mail.Read"},
	})
	if err != nil {
		t.Fatalf("MetadataEndpoint: %v", err)
	}
	if path != "/.well-known/oauth-protected-resource" {
		t.Errorf("path = %q, want /.well-known/oauth-protected-resource", path)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var m prm.Metadata
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Resource != "https://mailbox.example.com" {
		t.Errorf("resource = %q", m.Resource)
	}

	// prm's handler answers non-GET/HEAD with 405.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

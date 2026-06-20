package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetadataHandler(t *testing.T) {
	h := MetadataHandler(Config{
		ResourceID:     "https://mailbox.example.com",
		Issuers:        []string{"https://idp.example.com"},
		RequiredScopes: []string{"Mail.Read"},
	})

	req := httptest.NewRequest(http.MethodGet, WellKnownProtectedResource, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var m ProtectedResourceMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Resource != "https://mailbox.example.com" {
		t.Errorf("resource = %q", m.Resource)
	}
	if len(m.AuthorizationServers) != 1 || m.AuthorizationServers[0] != "https://idp.example.com" {
		t.Errorf("authorization_servers = %v", m.AuthorizationServers)
	}
	if len(m.ScopesSupported) != 1 || m.ScopesSupported[0] != "Mail.Read" {
		t.Errorf("scopes_supported = %v", m.ScopesSupported)
	}
	if len(m.BearerMethodsSupported) != 1 || m.BearerMethodsSupported[0] != "header" {
		t.Errorf("bearer_methods_supported = %v", m.BearerMethodsSupported)
	}
}

func TestMetadataHandlerRejectsNonGET(t *testing.T) {
	h := MetadataHandler(Config{ResourceID: "https://r.example"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, WellKnownProtectedResource, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

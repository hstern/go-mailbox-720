package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	h, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// decodeGraphError reads the response as a Graph error object and returns its code.
func decodeGraphError(t *testing.T, resp *http.Response) string {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var ge struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ge); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return ge.Error.Code
}

func TestUnimplementedOperationReturnsGraphError(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1.0/me/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
	if code := decodeGraphError(t, resp); code != "notImplemented" {
		t.Errorf("error code = %q, want notImplemented", code)
	}
}

func TestUnroutedPathReturnsGraphNotFound(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1.0/does/not/exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if code := decodeGraphError(t, resp); code != "notFound" {
		t.Errorf("error code = %q, want notFound", code)
	}
}

// A request missing the /v1.0 prefix must not route to an operation: the base
// path is part of the contract the conformance harness depends on.
func TestMissingBasePathDoesNotRoute(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/me/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d (unprefixed path must not match)", resp.StatusCode, http.StatusNotFound)
	}
}

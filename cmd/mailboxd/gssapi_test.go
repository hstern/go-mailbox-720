package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
)

// TestGSSAPIClientNoCCache: a credential minted for AP-REQ output (no ccache)
// can't build a GSSAPI client — gssapiClient surfaces ErrNoCCache, wrapped.
func TestGSSAPIClientNoCCache(t *testing.T) {
	cred := kerb.NewCredential("alice", testSPN, time.Unix(1<<40, 0), nil, []byte("apreq"))
	_, err := gssapiClient(cred, testSPN, "alice")
	if err == nil {
		t.Fatal("gssapiClient with a ccache-less credential: want error, got nil")
	}
	if !errors.Is(err, kerb.ErrNoCCache) {
		t.Errorf("err = %v, want it to wrap kerb.ErrNoCCache", err)
	}
	if !strings.Contains(err.Error(), "gssapi: ccache:") {
		t.Errorf("err = %v, want it wrapped with %q", err, "gssapi: ccache:")
	}
}

// TestGSSAPIClientBadCCache: malformed ccache bytes fail to unmarshal, wrapped
// with the parse-ccache context.
func TestGSSAPIClientBadCCache(t *testing.T) {
	cred := kerb.NewCredential("alice", testSPN, time.Unix(1<<40, 0), []byte("not-a-ccache"), nil)
	_, err := gssapiClient(cred, testSPN, "alice")
	if err == nil {
		t.Fatal("gssapiClient with garbage ccache bytes: want error, got nil")
	}
	if !strings.Contains(err.Error(), "gssapi: parse ccache:") {
		t.Errorf("err = %v, want it wrapped with %q", err, "gssapi: parse ccache:")
	}
}

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
)

// TestBearerIMAPOptionsManageSieve: the OAUTHBEARER per-identity provider enables
// ManageSieve exactly when an address is configured (MB720-60), without an explicit
// SASL factory (the imap package derives OAUTHBEARER from the bearer token).
func TestBearerIMAPOptionsManageSieve(t *testing.T) {
	o := bearerIMAPOptions(true, "tok", "", true)
	if o.ManageSieve != nil {
		t.Errorf("no sieve addr: ManageSieve = %+v, want nil", o.ManageSieve)
	}
	if o.BearerToken != "tok" || !o.TLS {
		t.Errorf("options = %+v", o)
	}
	o = bearerIMAPOptions(false, "tok", "sieve:4190", false)
	ms := o.ManageSieve
	if ms == nil || ms.Addr != "sieve:4190" || ms.STARTTLS || ms.SASL != nil {
		t.Errorf("ManageSieve = %+v, want Addr sieve:4190, no STARTTLS, no SASL factory", ms)
	}
}

var testSieveSPN = kerb.ServicePrincipal{Service: "sieve", Host: "mail.example.com", Realm: "EXAMPLE.COM"}

func TestSieveGSSAPIOptions(t *testing.T) {
	ex := &fakeKerbExchanger{err: errors.New("no ticket")}
	tok := func(context.Context) (string, bool) { return testRaw, true }
	o := sieveGSSAPIOptions(ex, tok, "sieve:4190", true, testSieveSPN, "alice")
	if o.Addr != "sieve:4190" || !o.STARTTLS || o.SASL == nil {
		t.Fatalf("options = %+v", o)
	}

	// The factory exchanges for a ccache against the SIEVE SPN (not the IMAP one),
	// on every call.
	if _, err := o.SASL(context.Background()); err == nil || !strings.Contains(err.Error(), "credential exchange") {
		t.Errorf("exchange failure = %v, want a wrapped credential-exchange error", err)
	}
	if ex.lastTarget != testSieveSPN || ex.lastOutput != kerb.OutputCCache {
		t.Errorf("exchange target/output = %v/%v, want %v/ccache", ex.lastTarget, ex.lastOutput, testSieveSPN)
	}

	// A credential that cannot become a GSSAPI client surfaces as an error.
	ex.err = nil
	ex.cred = kerb.NewCredential("alice", testSieveSPN, time.Unix(1<<40, 0), []byte("not-a-ccache"), nil)
	if _, err := o.SASL(context.Background()); err == nil || !strings.Contains(err.Error(), "gssapi: parse ccache") {
		t.Errorf("bad ccache = %v, want gssapi parse error", err)
	}
	if ex.callCount() != 2 {
		t.Errorf("exchange calls = %d, want one per ManageSieve connection (2)", ex.callCount())
	}

	// No bearer token on the context: fail before exchanging.
	o = sieveGSSAPIOptions(ex, func(context.Context) (string, bool) { return "", false }, "sieve:4190", true, testSieveSPN, "alice")
	if _, err := o.SASL(context.Background()); err == nil || ex.callCount() != 2 {
		t.Errorf("missing token: err = %v, calls = %d", err, ex.callCount())
	}
}

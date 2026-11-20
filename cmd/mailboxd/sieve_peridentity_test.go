package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	saslgssapi "github.com/hstern/go-sasl-gssapi"
	"github.com/hstern/krb5/iana/etypeID"
	"github.com/hstern/krb5/iana/nametype"
	"github.com/hstern/krb5/keytab"
	"github.com/hstern/krb5/types"

	"github.com/hstern/go-mailbox-720/internal/auth"
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

func TestValidateSieveFlags(t *testing.T) {
	cases := []struct {
		name                    string
		addr, sieveSPN, imapSPN string
		wantErr                 string // substring; empty = valid
	}{
		{"nothing configured", "", "", "", ""},
		{"static IMAP with sieve addr", "s:4190", "", "", ""},
		{"gssapi with addr and spn", "s:4190", "sieve/h", "imap/h", ""},
		{"gssapi without sieve at all", "", "", "imap/h", ""},
		{"spn without addr (static)", "", "sieve/h", "", "requires -mail-managesieve-addr"},
		{"spn without addr (gssapi)", "", "sieve/h", "imap/h", "requires -mail-managesieve-addr"},
		{"spn with static IMAP or audience", "s:4190", "sieve/h", "", "only applies with per-identity GSSAPI"},
		{"gssapi addr without spn", "s:4190", "", "imap/h", "requires -mail-managesieve-spn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSieveFlags(tc.addr, tc.sieveSPN, tc.imapSPN)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// mintSieveCredential mints a real holder-of-key ccache credential for spn the way
// kerbexchanged does: a service ticket encrypted under the service's keytab key,
// serialised into an MIT ccache.
func mintSieveCredential(t *testing.T, spn kerb.ServicePrincipal, subject string) *kerb.Credential {
	t.Helper()
	kt := keytab.New()
	if err := kt.AddEntry(spn.Service+"/"+spn.Host, spn.Realm, "svc-secret", time.Now(), 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
		t.Fatalf("keytab: %v", err)
	}
	now := time.Now()
	mt, err := kerb.NewDirectMinter(kerb.NewKeytabSource(kt, spn.Realm), spn.Realm).Mint(spn, kerb.MintOptions{
		ClientName:  types.PrincipalName{NameType: nametype.KRB_NT_PRINCIPAL, NameString: []string{subject}},
		ClientRealm: spn.Realm,
		AuthTime:    now,
		EndTime:     now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	cc, err := kerb.MarshalCCache(mt)
	if err != nil {
		t.Fatalf("marshal ccache: %v", err)
	}
	return kerb.NewCredential(subject, spn, mt.EndTime, cc, nil)
}

// TestSieveGSSAPIOptionsHappyPath: with a real holder-of-key ccache the factory
// yields a working GSSAPI client whose Start produces an initial response, and the
// exchange targeted the sieve SPN for a ccache. Each call yields a fresh client.
func TestSieveGSSAPIOptionsHappyPath(t *testing.T) {
	ex := &fakeKerbExchanger{cred: mintSieveCredential(t, testSieveSPN, "alice")}
	o := sieveGSSAPIOptions(ex, func(context.Context) (string, bool) { return testRaw, true }, "sieve:4190", false, testSieveSPN, "alice")

	sc, err := o.SASL(context.Background())
	if err != nil {
		t.Fatalf("SASL factory: %v", err)
	}
	if _, ok := sc.(*saslgssapi.Client); !ok {
		t.Fatalf("client is %T, want *saslgssapi.Client", sc)
	}
	mech, ir, err := sc.Start()
	if err != nil || mech != "GSSAPI" || len(ir) == 0 {
		t.Fatalf("Start = %q, %d bytes, %v; want GSSAPI with an AP-REQ initial response", mech, len(ir), err)
	}
	if ex.lastTarget != testSieveSPN || ex.lastOutput != kerb.OutputCCache {
		t.Errorf("exchange target/output = %v/%v, want %v/ccache", ex.lastTarget, ex.lastOutput, testSieveSPN)
	}
	sc2, err := o.SASL(context.Background())
	if err != nil || sc2 == sc {
		t.Errorf("second call = %v, %v; want a fresh client", sc2, err)
	}
}

// tokenRecordingExchanger records the raw token it is asked to exchange.
type tokenRecordingExchanger struct {
	fakeKerbExchanger
	raw string
}

func (e *tokenRecordingExchanger) Exchange(ctx context.Context, raw string, target kerb.ServicePrincipal, out kerb.OutputType) (*kerb.Credential, error) {
	e.raw = raw
	return e.fakeKerbExchanger.Exchange(ctx, raw, target, out)
}

// TestSieveGSSAPIOptionsRawTokenFromAuthContext: production wiring passes
// auth.RawToken; prove the bearer token a real auth middleware stored on the
// request context is what reaches the credential exchange.
func TestSieveGSSAPIOptionsRawTokenFromAuthContext(t *testing.T) {
	const aud = "mailbox"
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "jwks_uri": issuer + "/jwks",
			"authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"introspection_endpoint":                issuer + "/introspect",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"active": r.FormValue("token") == "opaque-user-token",
			"aud": aud, "scope": "Mail.Read", "sub": "alice"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	a, err := auth.New(context.Background(), auth.Config{
		Issuers: []string{issuer}, Audience: aud, RequiredScopes: []string{"Mail.Read"},
		Introspection: &auth.IntrospectionConfig{ClientID: "rs", ClientSecret: "s"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	ex := &tokenRecordingExchanger{fakeKerbExchanger: fakeKerbExchanger{cred: mintSieveCredential(t, testSieveSPN, "alice")}}
	o := sieveGSSAPIOptions(ex, auth.RawToken, "sieve:4190", false, testSieveSPN, "alice")
	var factoryErr error
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, factoryErr = o.SASL(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer opaque-user-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("middleware status = %d: %s", rec.Code, rec.Body)
	}
	if factoryErr != nil {
		t.Fatalf("factory: %v", factoryErr)
	}
	if ex.raw != "opaque-user-token" {
		t.Errorf("exchanged raw token = %q, want the request's bearer token", ex.raw)
	}
}

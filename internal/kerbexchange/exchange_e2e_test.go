//go:build kerberos_e2e

// This end-to-end test exercises the real OAuth2→Kerberos path: a real
// go-oauth2-kerberos-exchange service (the kerbexchanged binary, built from the
// dependency module) validates a JWT against an in-process JWKS issuer and mints
// a real Kerberos credential, which this test then validates cryptographically
// by decrypting the issued service ticket with the shared keytab key and
// asserting the client principal is the token subject. No KDC daemon and no
// Docker are needed: the library mints tickets offline from held service keys,
// so "real Kerberos" here means real keytab keys, real ticket encryption, and a
// real signed PAC — all checked in-Go.
//
// Gated behind the kerberos_e2e build tag (it builds and runs an external binary)
// so the default unit run stays hermetic and fast. Run with:
//
//	go test -tags kerberos_e2e -run TestKerberosExchangeEndToEnd ./internal/kerbexchange/...
package kerbexchange

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	"github.com/hstern/krb5/credentials"
	"github.com/hstern/krb5/iana/etypeID"
	"github.com/hstern/krb5/iana/nametype"
	"github.com/hstern/krb5/keytab"
	"github.com/hstern/krb5/messages"
	"github.com/hstern/krb5/types"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const (
	e2eRealm     = "EXAMPLE.COM"
	e2eService   = "imap"
	e2eHost      = "mail.example.com"
	e2eSubject   = "alice"
	e2ePassword  = "e2e-service-secret"
	e2eDomainSID = "S-1-5-21-1111111111-2222222222-3333333333"
)

func TestKerberosExchangeEndToEnd(t *testing.T) {
	ktPath, kt := writeE2EKeytab(t)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, ktPath, issuer.jwksURL)

	userJWT := issuer.mint(t, e2eSubject, time.Now().Add(10*time.Minute))
	target := kerb.ServicePrincipal{Service: e2eService, Host: e2eHost, Realm: e2eRealm}
	ex := New(endpoint, nil)

	// ccache output: validate the issued ticket cryptographically.
	t.Run("ccache", func(t *testing.T) {
		cred, err := ex.Exchange(context.Background(), userJWT, target, kerb.OutputCCache)
		if err != nil {
			t.Fatalf("Exchange (ccache): %v", err)
		}
		ccBytes, err := cred.CCache()
		if err != nil {
			t.Fatalf("CCache: %v", err)
		}
		tkt := serviceTicketFromCCache(t, ccBytes)

		spn := types.PrincipalName{NameType: nametype.KRB_NT_PRINCIPAL, NameString: []string{e2eService, e2eHost}}
		if err := tkt.DecryptEncPart(kt, &spn); err != nil {
			t.Fatalf("decrypt issued ticket with the shared keytab key: %v", err)
		}
		if got := tkt.DecryptedEncPart.CName.PrincipalNameString(); got != e2eSubject {
			t.Errorf("ticket client principal = %q, want %q (the token subject)", got, e2eSubject)
		}
		if got := tkt.DecryptedEncPart.CRealm; got != e2eRealm {
			t.Errorf("ticket client realm = %q, want %q", got, e2eRealm)
		}
		hasPAC, _, err := tkt.GetPACType(kt, &spn, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatalf("GetPACType: %v", err)
		}
		if !hasPAC {
			t.Error("issued ticket carries no PAC, want a synthetic PAC")
		}
	})

	// AP-REQ output: validate it is a well-formed GSSAPI initial-context token.
	// Uses a distinct subject so it does not share the kerbexchanged service's
	// (subject, SPN) cache entry with the ccache subtest — that server-side cache
	// keys only on subject and SPN, not the output type, so a ccache then an
	// AP-REQ request for the same subject+SPN would return the cached ccache-only
	// credential (a go-oauth2-kerberos-exchange behavior, tracked separately).
	t.Run("apreq", func(t *testing.T) {
		carolJWT := issuer.mint(t, "carol", time.Now().Add(10*time.Minute))
		cred, err := ex.Exchange(context.Background(), carolJWT, target, kerb.OutputAPReq)
		if err != nil {
			t.Fatalf("Exchange (apreq): %v", err)
		}
		apreq, err := cred.APReq()
		if err != nil {
			t.Fatalf("APReq: %v", err)
		}
		// RFC 2743 §3.1 initial-context token: 0x60 ‖ DER-length ‖ krb5-mech-OID ‖ …
		if len(apreq) == 0 || apreq[0] != 0x60 {
			t.Fatalf("AP-REQ token does not start with the GSSAPI token tag 0x60 (got % x)", first(apreq, 4))
		}
		krb5MechOID := []byte{0x06, 0x09, 0x2A, 0x86, 0x48, 0x86, 0xF7, 0x12, 0x01, 0x02, 0x02}
		if !bytes.Contains(apreq, krb5MechOID) {
			t.Error("AP-REQ token does not carry the Kerberos 5 GSSAPI mechanism OID")
		}
	})

	// A token for a different subject yields a ticket for a different principal,
	// proving the credential is minted per-identity, not from a shared account.
	t.Run("per-identity", func(t *testing.T) {
		bobJWT := issuer.mint(t, "bob", time.Now().Add(10*time.Minute))
		cred, err := ex.Exchange(context.Background(), bobJWT, target, kerb.OutputCCache)
		if err != nil {
			t.Fatalf("Exchange (bob): %v", err)
		}
		ccBytes, err := cred.CCache()
		if err != nil {
			t.Fatalf("CCache: %v", err)
		}
		tkt := serviceTicketFromCCache(t, ccBytes)
		spn := types.PrincipalName{NameType: nametype.KRB_NT_PRINCIPAL, NameString: []string{e2eService, e2eHost}}
		if err := tkt.DecryptEncPart(kt, &spn); err != nil {
			t.Fatalf("decrypt bob's ticket: %v", err)
		}
		if got := tkt.DecryptedEncPart.CName.PrincipalNameString(); got != "bob" {
			t.Errorf("ticket client principal = %q, want %q", got, "bob")
		}
	})
}

// writeE2EKeytab builds a keytab holding the service key (encrypts the ticket)
// and the krbtgt key (signs the PAC KDC signature) — both derived from one
// password — writes it to a temp file, and returns the path and the in-memory
// keytab for decryption-side assertions. Mirrors the library's interop fixture.
func writeE2EKeytab(t *testing.T) (string, *keytab.Keytab) {
	t.Helper()
	kt := keytab.New()
	now := time.Now()
	for _, pn := range []string{e2eService + "/" + e2eHost, "krbtgt/" + e2eRealm} {
		if err := kt.AddEntry(pn, e2eRealm, e2ePassword, now, 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
			t.Fatalf("keytab AddEntry %q: %v", pn, err)
		}
	}
	path := filepath.Join(t.TempDir(), "krb5.keytab")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create keytab: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := kt.Write(f); err != nil {
		t.Fatalf("write keytab: %v", err)
	}
	return path, kt
}

// serviceTicketFromCCache parses an MIT ccache and returns the first credential's
// service ticket.
func serviceTicketFromCCache(t *testing.T, ccBytes []byte) messages.Ticket {
	t.Helper()
	var cc credentials.CCache
	if err := cc.Unmarshal(ccBytes); err != nil {
		t.Fatalf("parse issued ccache: %v", err)
	}
	for _, e := range cc.GetEntries() {
		if len(e.Ticket) == 0 {
			continue
		}
		var tkt messages.Ticket
		if err := tkt.Unmarshal(e.Ticket); err != nil {
			continue
		}
		return tkt
	}
	t.Fatal("issued ccache holds no service ticket")
	return messages.Ticket{}
}

// jwksIssuer is an in-process JWT issuer: it serves a JWKS document and mints
// RS256-signed tokens the kerbexchanged JWKS validator accepts.
type jwksIssuer struct {
	key     jwk.Key // private signing key
	jwksURL string
}

func newJWKSIssuer(t *testing.T) *jwksIssuer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	key, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatalf("import private jwk: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, "e2e-key"); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := key.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set alg: %v", err)
	}
	pub, err := key.PublicKey()
	if err != nil {
		t.Fatalf("derive public jwk: %v", err)
	}
	// The validator's algorithm allowlist is enforced against the JWKS key's alg
	// field, so the published key must carry alg=RS256.
	if err := pub.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set public alg: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pub); err != nil {
		t.Fatalf("add public key to set: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &jwksIssuer{key: key, jwksURL: srv.URL + "/jwks.json"}
}

func (i *jwksIssuer) mint(t *testing.T, subject string, expiry time.Time) string {
	t.Helper()
	tok, err := jwt.NewBuilder().
		Subject(subject).
		IssuedAt(time.Now()).
		Expiration(expiry).
		Build()
	if err != nil {
		t.Fatalf("build JWT: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, i.key))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return string(signed)
}

// startKerbexchanged builds the kerbexchanged binary from the dependency module,
// runs it against the given keytab and JWKS URL, waits until it is listening, and
// returns its /token endpoint URL. The process is killed on test cleanup.
func startKerbexchanged(t *testing.T, keytabPath, jwksURL string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kerbexchanged")
	build := exec.Command("go", "build", "-o", bin, "github.com/hstern/go-oauth2-kerberos-exchange/cmd/kerbexchanged")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build kerbexchanged: %v", err)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin,
		"-addr", addr,
		"-token-path", "/token",
		"-keytab", keytabPath,
		"-realm", e2eRealm,
		"-jwks-url", jwksURL,
		"-domain-sid", e2eDomainSID,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kerbexchanged: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// The JWKS fetch happens before ListenAndServe, so once the port accepts a
	// connection the validator is ready.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return "http://" + addr + "/token"
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("kerbexchanged did not start listening on %s within the deadline", addr)
	return ""
}

// freeAddr returns a currently-free 127.0.0.1 host:port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func first(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

//go:build dockertest

package e2ekerberos

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

const e2eDomainSID = "S-1-5-21-1111111111-2222222222-3333333333"

// jwksIssuer is an in-process JWT issuer: it serves a JWKS document and mints
// RS256-signed tokens the kerbexchanged JWKS validator accepts. Copied from the
// MB720-45 foundation e2e.
type jwksIssuer struct {
	key     jwk.Key
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
// returns its /token endpoint URL. The process is killed on cleanup. Adapted from
// the MB720-45 foundation e2e.
func startKerbexchanged(t *testing.T, keytabPath, jwksURL string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kerbexchanged")
	build := exec.Command("go", "build", "-o", bin, "github.com/hstern/go-oauth2-kerberos-exchange/cmd/kerbexchanged")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build kerbexchanged: %v", err)
	}

	addr := "127.0.0.1:" + freePort(t)
	cmd := exec.Command(bin,
		"-addr", addr,
		"-token-path", "/token",
		"-keytab", keytabPath,
		"-realm", realm,
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

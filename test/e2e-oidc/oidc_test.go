// Package e2e is a black-box OIDC integration test: it stands up a real Kanidm
// IdP in a container, provisions an OAuth2 resource server, runs the real
// mailboxd against it, and asserts that a Kanidm-issued bearer token is accepted
// (reaching the not-yet-implemented handler, 501) while an unauthenticated
// request is rejected (401).
//
// Kanidm issues OPAQUE access tokens, so this exercises the RFC 7662
// introspection path of internal/auth end to end against a real IdP. It is a
// separate module (no extra deps on the library) and drives everything over HTTP
// — the orchestration is plain Go + the docker CLI, no shell or Python scripts.
//
// The test self-skips when docker is unavailable. mailboxd is built from the
// parent module, which must have run `go generate ./internal/graph` first.
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	kanidmImage   = "kanidm/server:1.4.6"
	toolsImage    = "kanidm/tools:1.4.6"
	containerName = "mailbox-e2e-kanidm"
	// Kanidm's configured origin is https://localhost:8443, and it rejects OIDC
	// discovery under any other host (issuer-URL match), so address it by the
	// same name everywhere. The leaf cert carries a localhost SAN.
	kanidmBase = "https://localhost:8443"

	rsClientID = "mailbox"        // the OAuth2 resource server we register
	rsScope    = "mail_read"      // a scope we require
	rsGroup    = "mailbox_admins" // group carrying the scope map
)

func TestOIDCEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // readable by the container's uid
		t.Fatal(err)
	}
	caPool := writeCerts(t, dir)
	writeServerConfig(t, dir)

	startKanidm(t, dir, caPool)
	adminPassword := recoverAdmin(t)
	secret := provision(t, dir, adminPassword)

	base := startMailboxd(t, dir, secret)
	kanidm := caClient(caPool)

	token := mintToken(t, kanidm, secret)

	// A valid Kanidm token reaches the (unimplemented) handler: 501, not 401.
	if got := status(t, base+"/me/messages", token); got != http.StatusNotImplemented {
		t.Errorf("authenticated request: status = %d, want 501 (token rejected by auth?)", got)
	}
	// No token is rejected by the middleware.
	if got := status(t, base+"/me/messages", ""); got != http.StatusUnauthorized {
		t.Errorf("unauthenticated request: status = %d, want 401", got)
	}
}

// writeCerts generates a CA and a localhost leaf signed by it, writes ca.pem /
// cert.pem / key.pem into dir, and returns a pool trusting the CA.
func writeCerts(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "kanidm"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	writePEM(t, filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER)
	writePEM(t, filepath.Join(dir, "cert.pem"), "CERTIFICATE", leafDER)
	writePEM(t, filepath.Join(dir, "key.pem"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey))

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return pool
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeServerConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := "bindaddress = \"[::]:8443\"\n" +
		"domain = \"localhost\"\n" +
		"origin = \"https://localhost:8443\"\n" +
		"db_path = \"/data/kanidm.db\"\n" +
		"tls_chain = \"/certs/cert.pem\"\n" +
		"tls_key = \"/certs/key.pem\"\n"
	if err := os.WriteFile(filepath.Join(dir, "server.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startKanidm(t *testing.T, dir string, caPool *x509.CertPool) {
	t.Helper()
	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	run(t, "docker", "run", "-d", "--name", containerName,
		"-v", filepath.Join(dir, "server.toml")+":/data/server.toml:ro",
		"-v", dir+":/certs:ro",
		"-p", "8443:8443",
		kanidmImage)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() })

	waitFor(t, "kanidm", 90*time.Second, func() bool {
		resp, err := caClient(caPool).Get(kanidmBase + "/status")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

var pwRe = regexp.MustCompile(`new_password:\s*"([^"]+)"`)

func recoverAdmin(t *testing.T) string {
	t.Helper()
	out := run(t, "docker", "exec", containerName, "kanidmd", "recover-account", "idm_admin", "-c", "/data/server.toml")
	m := pwRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse recovered password from:\n%s", out)
	}
	return m[1]
}

var secretRe = regexp.MustCompile(`SECRET=(\S+)`)

// provision logs in as idm_admin and registers the resource server, its group,
// and a scope map, returning the client's basic secret.
func provision(t *testing.T, dir, adminPassword string) string {
	t.Helper()
	script := strings.Join([]string{
		"set -e",
		"kanidm login -D idm_admin >/dev/null 2>&1",
		fmt.Sprintf("kanidm system oauth2 create %s %q https://localhost:8443 >/dev/null", rsClientID, "Mailbox API"),
		fmt.Sprintf("kanidm group create %s >/dev/null", rsGroup),
		fmt.Sprintf("kanidm group add-members %s %s >/dev/null", rsGroup, rsClientID),
		fmt.Sprintf("kanidm system oauth2 update-scope-map %s %s openid %s >/dev/null", rsClientID, rsGroup, rsScope),
		fmt.Sprintf("echo SECRET=$(kanidm system oauth2 show-basic-secret %s)", rsClientID),
	}, "\n")

	out := run(t, "docker", "run", "--rm", "--network", "host",
		"-v", dir+":/certs:ro",
		"-e", "KANIDM_URL="+kanidmBase,
		"-e", "KANIDM_CA_PATH=/certs/ca.pem",
		"-e", "KANIDM_PASSWORD="+adminPassword,
		toolsImage, "sh", "-c", script)
	m := secretRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse basic secret from:\n%s", out)
	}
	return m[1]
}

// startMailboxd builds and runs the server with auth enforced against Kanidm,
// trusting the test CA, and returns its /v1.0 base URL.
func startMailboxd(t *testing.T, dir, secret string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mailboxd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/mailboxd")
	build.Dir = "../.."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build mailboxd (did you run `go generate ./internal/graph`?): %v", err)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin,
		"-addr", addr,
		"-auth-issuer", kanidmBase+"/oauth2/openid/"+rsClientID,
		"-auth-audience", rsClientID,
		"-auth-scope", rsScope,
		"-auth-introspect-client-id", rsClientID,
	)
	cmd.Env = append(os.Environ(),
		"SSL_CERT_FILE="+filepath.Join(dir, "ca.pem"), // trust Kanidm's CA for discovery/introspection
		"MAILBOXD_INTROSPECT_CLIENT_SECRET="+secret,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mailboxd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	base := "http://" + addr + "/v1.0"
	waitFor(t, "mailboxd", 30*time.Second, func() bool {
		resp, err := http.Get(base + "/me/messages") // 401 once up (auth on, no token)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	})
	return base
}

func mintToken(t *testing.T, client *http.Client, secret string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"openid " + rsScope}}
	req, err := http.NewRequest(http.MethodPost, kanidmBase+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(rsClientID, secret)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatalf("decode token response: %v (%s)", err, body)
	}
	if tok.AccessToken == "" {
		t.Fatalf("empty access_token in: %s", body)
	}
	return tok.AccessToken
}

// status issues GET url (with an optional bearer token) and returns the status.
func status(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func caClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready within %s", what, timeout)
}

// run executes a command, failing the test (with output) on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

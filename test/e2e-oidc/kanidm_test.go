// kanidm.go is the Kanidm implementation of the idp interface. Kanidm issues opaque
// access tokens validated by RFC 7662 introspection over HTTPS (with a private CA).
// Its RFC 8693 token exchange is service-account only and cannot impersonate an end
// user, so kanidmIDP reports impersonation=false and is excluded from impersonation
// testing (MB720-15 / MB720-41).
package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

const (
	kanidmImage   = "kanidm/server:1.4.6"
	toolsImage    = "kanidm/tools:1.4.6"
	containerName = "mailbox-e2e-kanidm"
	// Kanidm's configured origin is https://localhost:8443, and it rejects OIDC
	// discovery under any other host (issuer-URL match), so address it by the same
	// name everywhere. The leaf cert carries a localhost SAN.
	kanidmBase = "https://localhost:8443"

	kClientID = "mailbox"        // the OAuth2 resource server we register
	kScope    = "mail_read"      // a scope we require
	kGroup    = "mailbox_admins" // group carrying the scope map
)

// kanidmIDP implements idp.
type kanidmIDP struct {
	dir           string
	caPool        *x509.CertPool
	adminPassword string
	secret        string
}

func (k *kanidmIDP) name() string               { return "kanidm" }
func (k *kanidmIDP) issuer() string             { return kanidmBase + "/oauth2/openid/" + kClientID }
func (k *kanidmIDP) audience() string           { return kClientID }
func (k *kanidmIDP) scope() string              { return kScope }
func (k *kanidmIDP) introspectClientID() string { return kClientID }
func (k *kanidmIDP) introspectSecret() string   { return k.secret }
func (k *kanidmIDP) sslCertFile() string        { return filepath.Join(k.dir, "ca.pem") }
func (k *kanidmIDP) caps() idpCaps              { return idpCaps{impersonation: false} }

func (k *kanidmIDP) start(t *testing.T) {
	t.Helper()
	k.dir = t.TempDir()
	if err := os.Chmod(k.dir, 0o755); err != nil { // readable by the container's uid
		t.Fatal(err)
	}
	k.caPool = writeCerts(t, k.dir)
	writeServerConfig(t, k.dir)

	_ = exec.Command("docker", "rm", "-f", containerName).Run()
	run(t, "docker", "run", "-d", "--name", containerName,
		"-v", filepath.Join(k.dir, "server.toml")+":/data/server.toml:ro",
		"-v", k.dir+":/certs:ro",
		"-p", "8443:8443",
		kanidmImage)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerName).Run() })

	waitFor(t, "kanidm", 90*time.Second, func() bool {
		resp, err := caClient(k.caPool).Get(kanidmBase + "/status")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	k.adminPassword = recoverAdmin(t)
}

var kanidmSecretRe = regexp.MustCompile(`SECRET=(\S+)`)

// provision logs in as idm_admin and registers the resource server, its group, and
// a scope map, storing the client's basic secret.
func (k *kanidmIDP) provision(t *testing.T) {
	t.Helper()
	script := "set -e\n" +
		"kanidm login -D idm_admin >/dev/null 2>&1\n" +
		"kanidm system oauth2 create " + kClientID + " 'Mailbox API' https://localhost:8443 >/dev/null\n" +
		"kanidm group create " + kGroup + " >/dev/null\n" +
		"kanidm group add-members " + kGroup + " " + kClientID + " >/dev/null\n" +
		"kanidm system oauth2 update-scope-map " + kClientID + " " + kGroup + " openid " + kScope + " >/dev/null\n" +
		"echo SECRET=$(kanidm system oauth2 show-basic-secret " + kClientID + ")"

	out := run(t, "docker", "run", "--rm", "--network", "host",
		"-v", k.dir+":/certs:ro",
		"-e", "KANIDM_URL="+kanidmBase,
		"-e", "KANIDM_CA_PATH=/certs/ca.pem",
		"-e", "KANIDM_PASSWORD="+k.adminPassword,
		toolsImage, "sh", "-c", script)
	m := kanidmSecretRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse basic secret from:\n%s", out)
	}
	k.secret = m[1]
}

func (k *kanidmIDP) mintToken(t *testing.T) string {
	t.Helper()
	return postToken(t, caClient(k.caPool), kanidmBase+"/oauth2/token",
		url.Values{"grant_type": {"client_credentials"}, "scope": {"openid " + kScope}},
		kClientID, k.secret)
}

var kanidmPwRe = regexp.MustCompile(`new_password:\s*"([^"]+)"`)

func recoverAdmin(t *testing.T) string {
	t.Helper()
	out := run(t, "docker", "exec", containerName, "kanidmd", "recover-account", "idm_admin", "-c", "/data/server.toml")
	m := kanidmPwRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("could not parse recovered password from:\n%s", out)
	}
	return m[1]
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

func caClient(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
}

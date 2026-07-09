//go:build dockertest

// Package e2ekerberos end-to-end tests the per-identity SASL GSSAPI path
// (MB720-46) against a real GSSAPI-speaking IMAP server: a stock Dovecot (which
// ships the GSSAPI auth mechanism) configured with a Kerberos service keytab.
//
// The OAuth2→Kerberos exchange (go-oauth2-kerberos-exchange / kerbexchanged) mints
// the user's service ticket offline from held service keys (holder-of-key), so no
// live KDC participates in authentication — that is the production shape. The test
// therefore shares ONE service keytab between kerbexchanged (which encrypts the
// ticket) and Dovecot (which decrypts the AP-REQ): identical key bytes on both
// sides is what makes the GSSAPI handshake succeed.
//
// Requires Docker; self-skips when it is unavailable. Run with:
//
//	go test -tags dockertest ./test/e2e-kerberos/
package e2ekerberos

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	saslgssapi "github.com/hstern/go-sasl-gssapi"
	"github.com/hstern/krb5/credentials"
	"github.com/hstern/krb5/iana/etypeID"
	"github.com/hstern/krb5/keytab"

	"github.com/hstern/go-mailbox-720/internal/kerbexchange"
	"github.com/hstern/go-mailbox-720/internal/mail/imap"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

const submissionService = "submission"

const (
	realm       = "EXAMPLE.COM"
	mailHost    = "mail.example.com"
	imapService = "imap"
	smtpService = "smtp"
	svcPassword = "e2e-service-secret"
	testSubject = "alice"

	dovecotImage = "dovecot/dovecot:2.3.21"
	dovecotCtr   = "mailbox-e2e-gssapi-dovecot"
)

// writeKeytab builds a keytab holding the IMAP and SMTP service keys (which
// encrypt the tickets) plus the krbtgt key (which signs the PAC KDC signature),
// all derived from one password, and returns its path. kerbexchanged and Dovecot
// both read this same file, so the ticket kerbexchanged encrypts is decryptable
// by Dovecot. Mirrors the foundation e2e's fixture.
func writeKeytab(t *testing.T, dir string) string {
	t.Helper()
	kt := keytab.New()
	now := time.Now()
	principals := []string{
		imapService + "/" + mailHost,
		smtpService + "/" + mailHost,
		// Dovecot's submission service derives its acceptor principal from the
		// protocol name ("submission"), so seed that too.
		"submission/" + mailHost,
		"krbtgt/" + realm,
	}
	for _, pn := range principals {
		if err := kt.AddEntry(pn, realm, svcPassword, now, 1, etypeID.AES256_CTS_HMAC_SHA1_96); err != nil {
			t.Fatalf("keytab AddEntry %q: %v", pn, err)
		}
	}
	path := filepath.Join(dir, "krb5.keytab")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create keytab: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := kt.Write(f); err != nil {
		t.Fatalf("write keytab: %v", err)
	}
	return path
}

const dovecotConf = `protocols = imap submission
ssl = no
disable_plaintext_auth = no
log_path = /dev/stderr
auth_verbose = yes
auth_debug = yes
mail_location = maildir:/srv/mail/%u/Maildir
auth_mechanisms = gssapi
auth_gssapi_hostname = ` + mailHost + `
auth_krb5_keytab = /etc/dovecot/dovecot.keytab
passdb {
  driver = static
  args = nopassword=y
}
userdb {
  driver = static
  args = uid=1000 gid=1000 home=/srv/mail/%u
}
service imap-login {
  inet_listener imap {
    port = 143
  }
}
service submission-login {
  inet_listener submission {
    port = 587
  }
}
# The submission service is a relay proxy; a relay host is required for it to
# start even though these tests only exercise the SASL GSSAPI AUTH handshake
# (which completes before any MAIL FROM), so the relay is never contacted.
submission_relay_host = 127.0.0.1
submission_relay_port = 24
`

const krb5Conf = `[libdefaults]
    default_realm = ` + realm + `
    dns_lookup_kdc = false
    dns_lookup_realm = false
    rdns = false
[realms]
    ` + realm + ` = {
        kdc = localhost:88
    }
[domain_realm]
    ` + mailHost + ` = ` + realm + `
    .example.com = ` + realm + `
`

// startDovecotGSSAPI writes the config + keytab into dir, starts a Dovecot
// container serving GSSAPI-authenticated IMAP (143) and submission/SMTP (587),
// waits for IMAP to listen, and returns the two host:port addresses. The
// container is removed on cleanup.
func startDovecotGSSAPI(t *testing.T, dir, keytabPath string) (imapAddr, smtpAddr string) {
	t.Helper()
	confPath := filepath.Join(dir, "dovecot.conf")
	if err := os.WriteFile(confPath, []byte(dovecotConf), 0o644); err != nil {
		t.Fatalf("write dovecot.conf: %v", err)
	}
	krbPath := filepath.Join(dir, "krb5.conf")
	if err := os.WriteFile(krbPath, []byte(krb5Conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}

	imapPort, smtpPort := freePort(t), freePort(t)
	_ = exec.Command("docker", "rm", "-f", dovecotCtr).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", dovecotCtr,
		"--hostname", mailHost,
		"-v", confPath+":/etc/dovecot/dovecot.conf:ro",
		"-v", keytabPath+":/etc/dovecot/dovecot.keytab:ro",
		"-v", krbPath+":/etc/krb5.conf:ro",
		"-p", "127.0.0.1:"+imapPort+":143",
		"-p", "127.0.0.1:"+smtpPort+":587",
		dovecotImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dovecot: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", dovecotCtr).Run() })

	imapAddr = "127.0.0.1:" + imapPort
	smtpAddr = "127.0.0.1:" + smtpPort
	waitForIMAP(t, imapAddr)
	return imapAddr, smtpAddr
}

// waitForIMAP blocks until the IMAP greeting is served or the deadline passes.
func waitForIMAP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			br := bufio.NewReader(conn)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, _ := br.ReadString('\n')
			_ = conn.Close()
			if strings.HasPrefix(line, "* OK") {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	dumpDovecotLogs(t)
	t.Fatalf("dovecot did not serve an IMAP greeting on %s in time", addr)
}

func dumpDovecotLogs(t *testing.T) {
	out, _ := exec.Command("docker", "logs", dovecotCtr).CombinedOutput()
	t.Logf("dovecot logs:\n%s", out)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return port
}

func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
}

// TestDovecotGSSAPICapability is the config smoke test: Dovecot comes up and
// advertises the GSSAPI SASL mechanism.
func TestDovecotGSSAPICapability(t *testing.T) {
	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	addr, _ := startDovecotGSSAPI(t, dir, kt)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err != nil { // greeting
		t.Fatalf("read greeting: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "a CAPABILITY\r\n"); err != nil {
		t.Fatalf("send CAPABILITY: %v", err)
	}
	var caps string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		caps += line
		if strings.HasPrefix(line, "a ") {
			break
		}
	}
	if !strings.Contains(caps, "AUTH=GSSAPI") {
		dumpDovecotLogs(t)
		t.Fatalf("CAPABILITY does not advertise AUTH=GSSAPI:\n%s", caps)
	}
}

// gssapiClientFor builds a SASL GSSAPI client from a kerbexchange credential,
// mirroring cmd/mailboxd's gssapiClient (which is unexported).
func gssapiClientFor(t *testing.T, cred *kerb.Credential, spn kerb.ServicePrincipal, authzID string) *saslgssapi.Client {
	t.Helper()
	b, err := cred.CCache()
	if err != nil {
		t.Fatalf("ccache: %v", err)
	}
	var cc credentials.CCache
	if err := cc.Unmarshal(b); err != nil {
		t.Fatalf("parse ccache: %v", err)
	}
	kc, err := saslgssapi.FromCCache(&cc)
	if err != nil {
		t.Fatalf("client from ccache: %v", err)
	}
	sc, err := saslgssapi.NewClient(saslgssapi.Config{Client: kc, Service: spn.Service + "/" + spn.Host, AuthzID: authzID})
	if err != nil {
		t.Fatalf("new gssapi client: %v", err)
	}
	return sc
}

// TestDovecotGSSAPIAuth is the crux: exchange the user's OAuth token for a
// Kerberos ccache (kerbexchanged, offline holder-of-key) and authenticate to the
// real Dovecot GSSAPI acceptor with it via the internal IMAP adapter's GSSAPI path.
func TestDovecotGSSAPIAuth(t *testing.T) {
	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, kt, issuer.jwksURL)
	addr, _ := startDovecotGSSAPI(t, dir, kt)

	userJWT := issuer.mint(t, testSubject, time.Now().Add(10*time.Minute))
	spn := kerb.ServicePrincipal{Service: imapService, Host: mailHost, Realm: realm}
	ex := kerbexchange.New(endpoint, nil)
	cred, err := ex.Exchange(context.Background(), userJWT, spn, kerb.OutputCCache)
	if err != nil {
		t.Fatalf("kerbexchange: %v", err)
	}

	sc := gssapiClientFor(t, cred, spn, testSubject)
	cl, err := imap.Dial(addr, testSubject, "", &imap.Options{TLS: false, GSSAPIClient: sc})
	if err != nil {
		dumpDovecotLogs(t)
		t.Fatalf("imap.Dial with GSSAPI against real Dovecot: %v", err)
	}
	defer func() { _ = cl.Close() }()

	// A command in the authenticated state proves the GSSAPI handshake bound the user.
	if _, err := cl.ListMailFolders(context.Background()); err != nil {
		dumpDovecotLogs(t)
		t.Fatalf("ListMailFolders after GSSAPI auth: %v", err)
	}
}

// TestDovecotGSSAPISMTPAuth exercises the SMTP submission GSSAPI path: exchange
// for a ccache against the submission SPN and authenticate to Dovecot's real
// submission (SMTP) service via the internal SMTP adapter's GSSAPI branch.
func TestDovecotGSSAPISMTPAuth(t *testing.T) {
	// Skipped: the GSSAPI initial response (an AP-REQ carrying kerbexchanged's
	// synthetic PAC, ~1.5-2 KB base64) exceeds SMTP's line limits — Dovecot
	// submission rejects the AUTH with "500 Line too long". SMTP has no
	// imap_max_line_length (64 KB) equivalent, and go-smtp inlines the initial
	// response on the AUTH command line rather than deferring it to a continuation
	// (RFC 4954 §4) — tracked upstream at emersion/go-smtp. The SMTP GSSAPI dial
	// path is covered by the internal/smtp unit test, and the GSSAPI handshake
	// against real Dovecot is proven by TestDovecotGSSAPIAuth (same sasl client).
	// The harness is kept so this can be revisited (e.g. a PAC-less/smaller ticket
	// or a submission server with larger line limits).
	t.Skip("SMTP submission GSSAPI: AP-REQ initial response exceeds SMTP line limits (see comment)")

	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, kt, issuer.jwksURL)
	_, smtpAddr := startDovecotGSSAPI(t, dir, kt)

	userJWT := issuer.mint(t, testSubject, time.Now().Add(10*time.Minute))
	spn := kerb.ServicePrincipal{Service: submissionService, Host: mailHost, Realm: realm}
	ex := kerbexchange.New(endpoint, nil)
	cred, err := ex.Exchange(context.Background(), userJWT, spn, kerb.OutputCCache)
	if err != nil {
		t.Fatalf("kerbexchange: %v", err)
	}

	sc := gssapiClientFor(t, cred, spn, testSubject)
	// A successful Dial means EHLO + AUTH GSSAPI completed against the real
	// submission server — the SMTP per-identity GSSAPI path end to end.
	cl, err := smtp.Dial(smtpAddr, testSubject, "", &smtp.Options{TLS: false, StartTLS: false, GSSAPIClient: sc})
	if err != nil {
		dumpDovecotLogs(t)
		t.Fatalf("smtp.Dial with GSSAPI against real Dovecot submission: %v", err)
	}
	_ = cl.Close()
}

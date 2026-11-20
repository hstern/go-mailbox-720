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

	"github.com/emersion/go-sasl"
	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	saslgssapi "github.com/hstern/go-sasl-gssapi"
	"github.com/hstern/krb5/credentials"
	"github.com/hstern/krb5/iana/etypeID"
	"github.com/hstern/krb5/keytab"

	"github.com/hstern/go-mailbox-720/internal/kerbexchange"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mail/imap"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

const (
	submissionService = "submission"
	sieveService      = "sieve"
)

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
		// ManageSieve (Pigeonhole) acceptor, MB720-60.
		sieveService + "/" + mailHost,
		// The CalDAV/CardDAV (Apache mod_auth_gssapi) acceptor's HTTP service key.
		httpService + "/" + davHost,
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

const dovecotConf = `protocols = imap submission sieve
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
service managesieve-login {
  inet_listener sieve {
    port = 4190
  }
}
plugin {
  sieve = file:/srv/mail/%u/sieve;active=/srv/mail/%u/.dovecot.sieve
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
// container serving GSSAPI-authenticated IMAP (143), submission/SMTP (587) and
// ManageSieve (4190), waits for IMAP to listen, and returns the three host:port
// addresses. The
// container is removed on cleanup.
func startDovecotGSSAPI(t *testing.T, dir, keytabPath string) (imapAddr, smtpAddr, sieveAddr string) {
	t.Helper()
	confPath := filepath.Join(dir, "dovecot.conf")
	if err := os.WriteFile(confPath, []byte(dovecotConf), 0o644); err != nil {
		t.Fatalf("write dovecot.conf: %v", err)
	}
	krbPath := filepath.Join(dir, "krb5.conf")
	if err := os.WriteFile(krbPath, []byte(krb5Conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}

	imapPort, smtpPort, sievePort := freePort(t), freePort(t), freePort(t)
	_ = exec.Command("docker", "rm", "-f", dovecotCtr).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", dovecotCtr,
		"--hostname", mailHost,
		"-v", confPath+":/etc/dovecot/dovecot.conf:ro",
		"-v", keytabPath+":/etc/dovecot/dovecot.keytab:ro",
		"-v", krbPath+":/etc/krb5.conf:ro",
		"-p", "127.0.0.1:"+imapPort+":143",
		"-p", "127.0.0.1:"+smtpPort+":587",
		"-p", "127.0.0.1:"+sievePort+":4190",
		dovecotImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dovecot: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", dovecotCtr).Run() })

	imapAddr = "127.0.0.1:" + imapPort
	smtpAddr = "127.0.0.1:" + smtpPort
	sieveAddr = "127.0.0.1:" + sievePort
	waitForIMAP(t, imapAddr)
	return imapAddr, smtpAddr, sieveAddr
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
	addr, _, _ := startDovecotGSSAPI(t, dir, kt)

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
	addr, _, _ := startDovecotGSSAPI(t, dir, kt)

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
	// Previously skipped: go-smtp inlined the ~1.5-2 KB AP-REQ initial response
	// on the AUTH command line, which Dovecot submission rejected as "Line too
	// long". go-smtp v0.26.0 (emersion/go-smtp#301, #307) defers an oversized
	// initial response to the first continuation (RFC 4954 §4).

	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, kt, issuer.jwksURL)
	_, smtpAddr, _ := startDovecotGSSAPI(t, dir, kt)

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

// TestDovecotGSSAPIManageSieve exercises per-identity ManageSieve (MB720-60)
// against Dovecot's real Pigeonhole ManageSieve service: IMAP authenticates with
// one GSSAPI credential (imap SPN) while ManageSieve authenticates, per connection,
// with a fresh GSSAPI client built from a second credential minted for the sieve
// SPN. Create/list/update/delete of a messageRule then round-trips a real Sieve
// script over the adapter, including the empty-continuation GSSAPI framing.
func TestDovecotGSSAPIManageSieve(t *testing.T) {
	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, kt, issuer.jwksURL)
	imapAddr, _, sieveAddr := startDovecotGSSAPI(t, dir, kt)

	userJWT := issuer.mint(t, testSubject, time.Now().Add(10*time.Minute))
	ex := kerbexchange.New(endpoint, nil)
	ctx := context.Background()
	imapSPN := kerb.ServicePrincipal{Service: imapService, Host: mailHost, Realm: realm}
	sieveSPN := kerb.ServicePrincipal{Service: sieveService, Host: mailHost, Realm: realm}
	imapCred, err := ex.Exchange(ctx, userJWT, imapSPN, kerb.OutputCCache)
	if err != nil {
		t.Fatalf("kerbexchange (imap): %v", err)
	}

	var sieveClients int
	cl, err := imap.Dial(imapAddr, testSubject, "", &imap.Options{
		TLS:          false,
		GSSAPIClient: gssapiClientFor(t, imapCred, imapSPN, testSubject),
		ManageSieve: &imap.ManageSieveOptions{
			Addr: sieveAddr, STARTTLS: false,
			// A fresh GSSAPI client per ManageSieve connection, as cmd/mailboxd does.
			SASL: func(ctx context.Context) (sasl.Client, error) {
				cred, err := ex.Exchange(ctx, userJWT, sieveSPN, kerb.OutputCCache)
				if err != nil {
					return nil, err
				}
				sieveClients++
				return gssapiClientFor(t, cred, sieveSPN, testSubject), nil
			},
		},
	})
	if err != nil {
		dumpDovecotLogs(t)
		t.Fatalf("imap.Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()

	fail := func(what string, err error) {
		t.Helper()
		dumpDovecotLogs(t)
		t.Fatalf("%s over GSSAPI ManageSieve: %v", what, err)
	}
	rules, err := cl.ListRules(ctx)
	if err != nil {
		fail("ListRules", err)
	}
	if len(rules) != 0 {
		t.Fatalf("fresh mailbox has rules: %+v", rules)
	}
	created, err := cl.CreateRule(ctx, mail.MessageRule{
		DisplayName: "From boss", Enabled: true,
		Conditions: mail.RuleConditions{SubjectContains: []string{"urgent"}},
		Actions:    mail.RuleActions{MoveToFolder: "Priority", MarkAsRead: true},
	})
	if err != nil {
		fail("CreateRule", err)
	}
	got, err := cl.ListRules(ctx)
	if err != nil || len(got) != 1 || got[0].DisplayName != "From boss" {
		t.Fatalf("ListRules after create = %+v, %v", got, err)
	}
	renamed := created
	renamed.DisplayName = "From the boss"
	if _, err := cl.UpdateRule(ctx, created.ID, renamed); err != nil {
		fail("UpdateRule", err)
	}
	if got, err = cl.ListRules(ctx); err != nil || len(got) != 1 || got[0].DisplayName != "From the boss" {
		t.Fatalf("ListRules after update = %+v, %v", got, err)
	}
	if err := cl.DeleteRule(ctx, created.ID); err != nil {
		fail("DeleteRule", err)
	}
	if got, err = cl.ListRules(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListRules after delete = %+v, %v", got, err)
	}
	if sieveClients < 5 {
		t.Errorf("only %d GSSAPI clients built; every ManageSieve operation must build its own", sieveClients)
	}
}

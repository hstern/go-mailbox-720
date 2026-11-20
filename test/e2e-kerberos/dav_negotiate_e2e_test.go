//go:build dockertest

// This file end-to-end tests the per-identity CalDAV/CardDAV SPNEGO path
// (MB720-47, MB720-59) against a replay-cache-enforcing acceptor: Apache httpd with
// mod_auth_gssapi (keytab-based, MIT krb5's default replay cache) fronting a real
// Radicale CalDAV+CardDAV server, with the authenticated principal delegated to
// Radicale via X-Remote-User.
//
// Before MB720-59 mailboxd sent one AP-REQ on every request; the acceptor accepted
// the first and rejected the rest as replays (HTTP 401). The adapters now mint a
// fresh SPNEGO token per HTTP request from the cached service ticket. As in the
// IMAP/SMTP e2e, kerbexchanged and the acceptor share ONE service keytab and no KDC
// participates.
package e2ekerberos

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/calendar/caldav"
	"github.com/hstern/go-mailbox-720/internal/contacts/carddav"
	"github.com/hstern/go-mailbox-720/internal/davauth"
	"github.com/hstern/go-mailbox-720/internal/kerbexchange"
)

const (
	httpService = "HTTP"
	davHost     = "dav.example.com"
	davImage    = "mailbox-e2e-gssapi-dav:1"
	davCtr      = "mailbox-e2e-gssapi-dav"
)

const davDockerfile = `FROM docker.io/library/debian:12.11-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      apache2 libapache2-mod-auth-gssapi radicale \
 && rm -rf /var/lib/apt/lists/* \
 && a2enmod auth_gssapi proxy proxy_http headers \
 && a2dissite 000-default
COPY apache.conf /etc/apache2/sites-available/dav.conf
COPY radicale.conf /etc/radicale/config
COPY start.sh /start.sh
RUN a2ensite dav && chmod +x /start.sh
CMD ["/start.sh"]
`

const davStartScript = `#!/bin/sh
mkdir -p /var/lib/radicale/collections
radicale --config /etc/radicale/config &
# Do not start answering (and so appear ready) until Radicale is listening.
until python3 -c 'import socket; socket.create_connection(("127.0.0.1", 5232), 1)' 2>/dev/null; do sleep 0.2; done
exec apache2ctl -D FOREGROUND
`

// radicale trusts the identity Apache authenticated (X-Remote-User) and nothing else.
const davRadicaleConf = `[server]
hosts = 127.0.0.1:5232
[auth]
type = http_x_remote_user
[rights]
type = owner_only
[logging]
level = debug
[storage]
filesystem_folder = /var/lib/radicale/collections
`

// apache enforces SPNEGO with the keytab-based default (replay-checking) acceptor
// and passes the realm-stripped principal to Radicale.
const davApacheConf = `ServerName ` + davHost + `
ErrorLog /dev/stderr
LogLevel info
<VirtualHost *:80>
    <Location />
        AuthType GSSAPI
        AuthName "Kerberos"
        GssapiCredStore keytab:/etc/apache2/http.keytab
        GssapiLocalName On
        Require valid-user
    </Location>
    # "set" overwrites any client-supplied X-Remote-User; the expr form reads
    # REMOTE_USER after authn.
    RequestHeader set X-Remote-User "expr=%{REMOTE_USER}"
    ProxyPreserveHost On
    ProxyPass / http://127.0.0.1:5232/
</VirtualHost>
`

// startDAVGSSAPI builds the Apache+Radicale image, runs it with the shared keytab,
// and returns its base URL once Apache answers (with a 401 Negotiate challenge).
func startDAVGSSAPI(t *testing.T, dir, keytabPath string) string {
	t.Helper()
	ctxDir := filepath.Join(dir, "davimage")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"Dockerfile":    davDockerfile,
		"apache.conf":   davApacheConf,
		"radicale.conf": davRadicaleConf,
		"start.sh":      davStartScript,
	} {
		if err := os.WriteFile(filepath.Join(ctxDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if out, err := exec.Command("docker", "build", "-t", davImage, ctxDir).CombinedOutput(); err != nil {
		t.Fatalf("docker build dav image: %v\n%s", err, out)
	}
	krbPath := filepath.Join(dir, "dav-krb5.conf")
	if err := os.WriteFile(krbPath, []byte(krb5Conf), 0o644); err != nil {
		t.Fatalf("write krb5.conf: %v", err)
	}

	port := freePort(t)
	_ = exec.Command("docker", "rm", "-f", davCtr).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", davCtr,
		"--hostname", davHost,
		"-v", keytabPath+":/etc/apache2/http.keytab:ro",
		"-v", krbPath+":/etc/krb5.conf:ro",
		"-p", "127.0.0.1:"+port+":80",
		davImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dav: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", davCtr).Run() })

	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "Negotiate" {
				return base
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	dumpDAVLogs(t)
	t.Fatalf("apache did not serve a Negotiate challenge on %s in time", base)
	return ""
}

func dumpDAVLogs(t *testing.T) {
	out, _ := exec.Command("docker", "logs", davCtr).CombinedOutput()
	t.Logf("dav container logs:\n%s", out)
}

// davRequest issues one request through client and returns the status and body.
func davRequest(t *testing.T, c *http.Client, method, url, contentType, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const (
	davMkCalendar = `<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:displayname>Work</D:displayname>
    <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`
	davMkAddressBook = `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
    <D:displayname>People</D:displayname>
  </D:prop></D:set>
</D:mkcol>`
	davEventICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//go-mailbox-720//dav-negotiate-e2e//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:evt-1@go-mailbox-720.test\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260615T090000Z\r\nDTEND:20260615T100000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	davContactVCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:contact-1@go-mailbox-720.test\r\n" +
		"FN:Ada Lovelace\r\nN:Lovelace;Ada;;;\r\nEMAIL:ada@example.com\r\nEND:VCARD\r\n"
)

// davFixture is a running Apache+Radicale acceptor plus the exchanged credential
// for the test user.
type davFixture struct {
	base string
	cred *kerb.Credential
	spn  kerb.ServicePrincipal
}

func newDAVFixture(t *testing.T) davFixture {
	t.Helper()
	dockerAvailable(t)
	dir := t.TempDir()
	kt := writeKeytab(t, dir)
	issuer := newJWKSIssuer(t)
	endpoint := startKerbexchanged(t, kt, issuer.jwksURL)
	base := startDAVGSSAPI(t, dir, kt)

	userJWT := issuer.mint(t, testSubject, time.Now().Add(10*time.Minute))
	spn := kerb.ServicePrincipal{Service: httpService, Host: davHost, Realm: realm}
	cred, err := kerbexchange.New(endpoint, nil).Exchange(context.Background(), userJWT, spn, kerb.OutputCCache)
	if err != nil {
		t.Fatalf("kerbexchange: %v", err)
	}
	return davFixture{base: base, cred: cred, spn: spn}
}

// TestDAVNegotiateMultipleRequests is the crux of MB720-59: one identity makes many
// CalDAV and CardDAV requests in sequence (discovery PROPFINDs, REPORTs, GETs, plus
// seeding writes) against an acceptor that rejects a replayed AP-REQ. Each must
// succeed because every HTTP request carries a freshly minted Negotiate token, built
// locally from the one exchanged ccache.
func TestDAVNegotiateMultipleRequests(t *testing.T) {
	f := newDAVFixture(t)
	src, err := davauth.KerberosTokenSource(f.cred, f.spn)
	if err != nil {
		t.Fatalf("KerberosTokenSource: %v", err)
	}
	ctx := context.Background()

	// Seed a calendar, an event, an address book and a contact through the same
	// token source (each write is its own request, so its own token).
	raw := &http.Client{Transport: nil}
	hc := davauth.NegotiateHTTPClient(raw, src).(*http.Client)
	user := f.base + "/" + testSubject
	for _, step := range []struct {
		method, path, ctype, body string
	}{
		{"MKCALENDAR", "/work/", "application/xml", davMkCalendar},
		{"PUT", "/work/evt-1.ics", "text/calendar", davEventICS},
		{"MKCOL", "/people/", "application/xml", davMkAddressBook},
		{"PUT", "/people/contact-1.vcf", "text/vcard", davContactVCard},
	} {
		if code, body := davRequest(t, hc, step.method, user+step.path, step.ctype, step.body); code != http.StatusCreated {
			dumpDAVLogs(t)
			t.Fatalf("%s %s = %d, want 201:\n%s", step.method, step.path, code, body)
		}
	}

	cal, err := caldav.Dial(f.base+"/", "", "", &caldav.Options{NegotiateToken: src})
	if err != nil {
		t.Fatalf("caldav.Dial: %v", err)
	}
	card, err := carddav.Dial(f.base+"/", "", "", &carddav.Options{NegotiateToken: src})
	if err != nil {
		t.Fatalf("carddav.Dial: %v", err)
	}

	// Repeat the whole read sequence so a replayed token has many chances to show.
	for round := 1; round <= 3; round++ {
		cals, err := cal.ListCalendars(ctx)
		if err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: ListCalendars: %v", round, err)
		}
		var work *calendar.Calendar
		for i := range cals {
			if cals[i].Name == "Work" {
				work = &cals[i]
			}
		}
		if work == nil {
			t.Fatalf("round %d: calendar Work not listed: %+v", round, cals)
		}
		evs, err := cal.ListEvents(ctx, work.ID, calendar.Range{})
		if err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: ListEvents: %v", round, err)
		}
		if len(evs) != 1 || evs[0].Title != "Standup" {
			t.Fatalf("round %d: events = %+v, want one Standup", round, evs)
		}
		if _, err := cal.GetEvent(ctx, evs[0].ID); err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: GetEvent: %v", round, err)
		}

		books, err := card.ListAddressBooks(ctx)
		if err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: ListAddressBooks: %v", round, err)
		}
		if len(books) != 1 {
			t.Fatalf("round %d: address books = %+v, want 1", round, books)
		}
		cs, err := card.ListContacts(ctx, books[0].ID)
		if err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: ListContacts: %v", round, err)
		}
		if len(cs) != 1 {
			t.Fatalf("round %d: contacts = %+v, want 1", round, cs)
		}
		if _, err := card.GetContact(ctx, cs[0].ID); err != nil {
			dumpDAVLogs(t)
			t.Fatalf("round %d: GetContact: %v", round, err)
		}
	}
}

// TestDAVAcceptorEnforcesReplayCache is the control for the test above: it proves
// the acceptor really rejects a re-presented AP-REQ, so the multi-request test is a
// meaningful regression guard rather than passing against a lenient server. One
// token minted once is sent twice: the first is accepted, the second is 401.
func TestDAVAcceptorEnforcesReplayCache(t *testing.T) {
	f := newDAVFixture(t)
	src, err := davauth.KerberosTokenSource(f.cred, f.spn)
	if err != nil {
		t.Fatalf("KerberosTokenSource: %v", err)
	}
	tok, err := src()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	send := func() int {
		req, _ := http.NewRequest("OPTIONS", f.base+"/", nil)
		req.Header.Set("Authorization", "Negotiate "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(); code >= 400 {
		dumpDAVLogs(t)
		t.Fatalf("first use of a fresh token = %d, want success", code)
	}
	if code := send(); code != http.StatusUnauthorized {
		dumpDAVLogs(t)
		t.Fatalf("replayed token = %d, want 401 (acceptor must enforce its replay cache)", code)
	}
	t.Log("replay cache enforced: second use of the same AP-REQ rejected with 401")
}

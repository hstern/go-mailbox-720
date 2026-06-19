package e2e

// Radicale (CalDAV) is the calendar backend half of the vertical slice. This
// file stands up a real Radicale server in Docker, seeds one calendar
// collection with one event via raw authenticated CalDAV requests (MKCALENDAR +
// PUT), and is consumed by TestOIDCEndToEnd to exercise GET /me/events through
// mailboxd's CalDAV port. It reimplements the recipe proven by
// internal/calendar/caldav/radicale_test.go, since this black-box module cannot
// import internal/.

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	radicaleImage = "tomsquest/docker-radicale:latest"
	radicaleCtr   = "mailbox-e2e-oidc-radicale"
	radicaleUser  = "test"
	radicalePass  = "testpass"

	// calendarName / calendarSlug are the display name and URL segment of the
	// seeded calendar collection (created under the principal /test/). The
	// handler's /me/events resolves the principal's first (default) calendar, so
	// a single collection makes the seeded event the one returned.
	calendarName = "Work"
	calendarSlug = "work"

	eventSummary = "Team Standup"

	// radicaleConfig is a minimal Radicale config: htpasswd auth (plain
	// encryption, so no hashing tooling is needed), owner-only rights, and
	// filesystem storage under /data (writable via TAKE_FILE_OWNERSHIP).
	radicaleConfig = `[server]
hosts = 0.0.0.0:5232

[auth]
type = htpasswd
htpasswd_filename = /config/users
htpasswd_encryption = plain

[rights]
type = owner_only

[storage]
filesystem_folder = /data/collections
`

	// radicaleUsers is the htpasswd file. With htpasswd_encryption=plain the
	// password is stored verbatim, keeping the fixture self-contained.
	radicaleUsers = radicaleUser + ":" + radicalePass + "\n"

	// mkcalendarBody requests a VEVENT calendar collection (MKCALENDAR is
	// RFC 4791 §5.3.1) and is issued as a raw request — no go-webdav dependency.
	mkcalendarBody = `<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:displayname>` + calendarName + `</D:displayname>
    <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`
)

// eventICS is the iCalendar object PUT into the calendar. RFC 5545 requires
// CRLF line endings, so the lines are joined with \r\n.
var eventICS = crlf(
	"BEGIN:VCALENDAR",
	"VERSION:2.0",
	"PRODID:-//go-mailbox-720//radicale-e2e//EN",
	"BEGIN:VEVENT",
	"UID:evt-1@go-mailbox-720.test",
	"DTSTAMP:20260101T000000Z",
	"DTSTART:20260615T090000Z",
	"DTEND:20260615T100000Z",
	"SUMMARY:"+eventSummary,
	"END:VEVENT",
	"END:VCALENDAR",
	"",
)

func crlf(lines ...string) string {
	var b bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
	}
	return b.String()
}

// startRadicale runs a Radicale container and returns its host CalDAV base URL
// (http://127.0.0.1:<port>/). The container is removed via t.Cleanup.
func startRadicale(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // readable by the container uid
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "config")
	if err := os.Mkdir(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config"), []byte(radicaleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "users"), []byte(radicaleUsers), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", radicaleCtr).Run()

	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	// Run as root with TAKE_FILE_OWNERSHIP so the entrypoint chowns the
	// (root-owned) /data bind-mount for the radicale user before dropping
	// privileges; the config mount is read-only.
	out, err := exec.Command("docker", "run", "-d", "--name", radicaleCtr,
		"-u", "0",
		"-e", "TAKE_FILE_OWNERSHIP=true",
		"-v", cfgDir+":/config:ro",
		"-p", "127.0.0.1:"+port+":5232",
		radicaleImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run radicale: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", radicaleCtr).Run() })

	base := "http://127.0.0.1:" + port + "/"
	waitRadicale(t, base)
	return base
}

// seedCalendar provisions one calendar collection with one event via raw CalDAV
// requests: MKCALENDAR (no go-webdav helper) then a PUT of the iCalendar object.
// Radicale does not auto-create the parent collection on a bare PUT, so the
// MKCALENDAR must come first.
func seedCalendar(t *testing.T, base string) {
	t.Helper()
	calURL := base + radicaleUser + "/" + calendarSlug + "/"
	doRadicale(t, "MKCALENDAR", calURL, "application/xml", mkcalendarBody, http.StatusCreated)
	doRadicale(t, http.MethodPut, calURL+"evt-1.ics", "text/calendar", eventICS, http.StatusCreated)
}

// doRadicale issues an authenticated CalDAV request and asserts the status code.
func doRadicale(t *testing.T, method, url, contentType, body string, wantStatus int) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("%s %s: new request: %v", method, url, err)
	}
	req.SetBasicAuth(radicaleUser, radicalePass)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d", method, url, resp.StatusCode, wantStatus)
	}
}

// waitRadicale blocks until the server answers an authenticated PROPFIND on the
// CalDAV root (a 207 Multi-Status), i.e. it is up and the htpasswd file is
// loaded.
func waitRadicale(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("PROPFIND", base, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(radicaleUser, radicalePass)
		req.Header.Set("Depth", "0")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusMultiStatus {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", radicaleCtr).CombinedOutput()
	t.Fatalf("radicale did not become ready in time\n%s", logs)
}

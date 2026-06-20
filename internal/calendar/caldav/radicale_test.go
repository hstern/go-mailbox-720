//go:build dockertest

// Integration test for the CalDAV adapter against a real Radicale server in
// Docker. Build-tagged so the default `go test ./...` (which exercises only the
// mapping layer against in-memory iCalendar fixtures) stays fast and
// dependency-free; run with:
//
//	go test -tags dockertest ./internal/calendar/caldav/
//
// Self-skips when docker is unavailable.
package caldav

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

const (
	radicaleImage = "tomsquest/docker-radicale:latest"
	radicaleCtr   = "mailbox-e2e-radicale"
	radicaleUser  = "test"
	radicalePass  = "testpass"

	// calendarName / calendarSlug are the display name and URL segment of the
	// seeded calendar collection (created under the principal /test/).
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
	// RFC 4791 §5.3.1; go-webdav's caldav client has no helper for it, so the
	// test issues it as a raw request).
	mkcalendarBody = `<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:displayname>` + calendarName + `</D:displayname>
    <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`
)

// seedEventStart is the DTSTART of the seeded event, asserted against the
// adapter's mapped Event.Start.
var seedEventStart = time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)

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

func TestRadicaleIntegration(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base := startRadicale(t)
	seedCalendar(t, base)

	cl, err := Dial(base, radicaleUser, radicalePass, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	cals, err := cl.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	var work *calendar.Calendar
	for i := range cals {
		if cals[i].Name == calendarName {
			work = &cals[i]
			break
		}
	}
	if work == nil {
		t.Fatalf("calendar %q not listed: %+v", calendarName, cals)
	}

	events, err := cl.ListEvents(ctx, work.ID, calendar.Range{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Subject != eventSummary {
		t.Errorf("Subject = %q, want %q", ev.Subject, eventSummary)
	}
	if !ev.Start.Equal(seedEventStart) {
		t.Errorf("Start = %v, want %v", ev.Start, seedEventStart)
	}

	got, err := cl.GetEvent(ctx, ev.ID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.Subject != eventSummary {
		t.Errorf("GetEvent Subject = %q, want %q", got.Subject, eventSummary)
	}
	if !got.Start.Equal(seedEventStart) {
		t.Errorf("GetEvent Start = %v, want %v", got.Start, seedEventStart)
	}
}

// TestRadicaleWrite exercises the Writer capability end to end against Radicale:
// CreateEvent into the seeded collection, confirm ListEvents/GetEvent surface it,
// UpdateEvent changes its subject and a re-read reflects it, then DeleteEvent
// removes it and a re-read no longer returns it.
func TestRadicaleWrite(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base := startRadicale(t)
	seedCalendar(t, base)

	cl, err := Dial(base, radicaleUser, radicalePass, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	// The adapter must advertise the write capability.
	w, ok := interface{}(cl).(calendar.Writer)
	if !ok {
		t.Fatal("*Client does not implement calendar.Writer")
	}

	cals, err := cl.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	var work *calendar.Calendar
	for i := range cals {
		if cals[i].Name == calendarName {
			work = &cals[i]
			break
		}
	}
	if work == nil {
		t.Fatalf("calendar %q not listed: %+v", calendarName, cals)
	}

	const createdSubject = "Sprint Review"
	createStart := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	created, err := w.CreateEvent(ctx, work.ID, calendar.Event{
		Subject:   createdSubject,
		Start:     createStart,
		End:       createStart.Add(time.Hour),
		Location:  "Room 7",
		Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
		Attendees: []calendar.Address{{Email: "bob@example.com"}},
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateEvent returned an event with no ID")
	}
	if created.UID == "" {
		t.Fatal("CreateEvent returned an event with no UID")
	}
	if created.CalendarID != work.ID {
		t.Errorf("created.CalendarID = %q, want %q", created.CalendarID, work.ID)
	}

	// ListEvents must now surface the created event alongside the seeded one.
	events, err := cl.ListEvents(ctx, work.ID, calendar.Range{})
	if err != nil {
		t.Fatalf("ListEvents after create: %v", err)
	}
	if findSubject(events, createdSubject) == nil {
		t.Fatalf("created event %q not in listing: %+v", createdSubject, events)
	}

	// GetEvent by the returned opaque ID must round-trip subject and start.
	got, err := cl.GetEvent(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetEvent after create: %v", err)
	}
	if got.Subject != createdSubject {
		t.Errorf("GetEvent Subject = %q, want %q", got.Subject, createdSubject)
	}
	if !got.Start.Equal(createStart) {
		t.Errorf("GetEvent Start = %v, want %v", got.Start, createStart)
	}

	// UpdateEvent changes the subject; the UID and ID are preserved so a re-read
	// reflects the new subject at the same resource.
	const updatedSubject = "Sprint Review (rescheduled)"
	updated := got
	updated.Subject = updatedSubject
	if _, err := w.UpdateEvent(ctx, updated); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	reread, err := cl.GetEvent(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetEvent after update: %v", err)
	}
	if reread.Subject != updatedSubject {
		t.Errorf("after update Subject = %q, want %q", reread.Subject, updatedSubject)
	}

	// DeleteEvent removes the resource; neither GetEvent nor ListEvents should
	// surface it afterwards.
	if err := w.DeleteEvent(ctx, created.ID); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	if _, err := cl.GetEvent(ctx, created.ID); err == nil {
		t.Error("GetEvent after delete returned no error, want a not-found error")
	}
	events, err = cl.ListEvents(ctx, work.ID, calendar.Range{})
	if err != nil {
		t.Fatalf("ListEvents after delete: %v", err)
	}
	if findSubject(events, updatedSubject) != nil {
		t.Errorf("deleted event %q still in listing: %+v", updatedSubject, events)
	}
}

// TestRadicaleDelta exercises the DeltaReader capability end to end against
// Radicale (which supports RFC 6578 sync-collection): an initial Delta with an
// empty token returns the seeded event and a non-empty sync-token; after a
// second event is created, an incremental Delta with that token returns the new
// event and an advanced token.
func TestRadicaleDelta(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base := startRadicale(t)
	seedCalendar(t, base)

	cl, err := Dial(base, radicaleUser, radicalePass, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	// The adapter must advertise the delta capability.
	d, ok := interface{}(cl).(calendar.DeltaReader)
	if !ok {
		t.Fatal("*Client does not implement calendar.DeltaReader")
	}

	cals, err := cl.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	var work *calendar.Calendar
	for i := range cals {
		if cals[i].Name == calendarName {
			work = &cals[i]
			break
		}
	}
	if work == nil {
		t.Fatalf("calendar %q not listed: %+v", calendarName, cals)
	}

	// Initial sync: an empty token returns the current events and a fresh token.
	initial, _, token, err := d.Delta(ctx, work.ID, "")
	if err != nil {
		t.Fatalf("initial Delta: %v", err)
	}
	if token == "" {
		t.Fatal("initial Delta returned an empty sync-token")
	}
	if findSubject(initial, eventSummary) == nil {
		t.Fatalf("initial Delta missing seeded event %q: %+v", eventSummary, initial)
	}

	// Create a second event so the incremental sync has a change to report.
	w, ok := interface{}(cl).(calendar.Writer)
	if !ok {
		t.Fatal("*Client does not implement calendar.Writer")
	}
	const deltaSubject = "Planning"
	deltaStart := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	if _, err := w.CreateEvent(ctx, work.ID, calendar.Event{
		Subject: deltaSubject,
		Start:   deltaStart,
		End:     deltaStart.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	// Incremental sync: the prior token returns only the new event and a token
	// that has advanced past the initial one.
	changed, _, next, err := d.Delta(ctx, work.ID, token)
	if err != nil {
		t.Fatalf("incremental Delta: %v", err)
	}
	if next == "" {
		t.Fatal("incremental Delta returned an empty sync-token")
	}
	if next == token {
		t.Errorf("incremental Delta token did not advance: %q", next)
	}
	if findSubject(changed, deltaSubject) == nil {
		t.Fatalf("incremental Delta missing new event %q: %+v", deltaSubject, changed)
	}
	if findSubject(changed, eventSummary) != nil {
		t.Errorf("incremental Delta unexpectedly re-reported unchanged event %q: %+v", eventSummary, changed)
	}
}

// findSubject returns the first event with the given subject, or nil.
func findSubject(events []calendar.Event, subject string) *calendar.Event {
	for i := range events {
		if events[i].Subject == subject {
			return &events[i]
		}
	}
	return nil
}

// startRadicale runs a Radicale container and returns its host CalDAV base URL
// (http://127.0.0.1:<port>/). The container is removed via t.Cleanup.
func startRadicale(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // readable by the container uid
		t.Fatal(err)
	}
	cfgDir := dir + "/config"
	if err := os.Mkdir(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgDir+"/config", []byte(radicaleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgDir+"/users", []byte(radicaleUsers), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", radicaleCtr).Run()

	port := freePort(t)
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
	t.Fatal(fmt.Sprintf("radicale did not become ready in time\n%s", logs))
}

// freePort reserves an ephemeral localhost port, releases it, and returns it for
// docker to bind. There is an inherent race between release and bind, but it
// matches the IMAP e2e test's approach and is fine for a single local server.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

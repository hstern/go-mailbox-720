//go:build dockertest

package caldav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/server"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

// autoScheduleProxy fronts the Radicale base URL and injects the RFC 6638
// calendar-auto-schedule compliance class into OPTIONS DAV response headers, so
// the adapter detects the (otherwise storage-only) server as a native scheduler
// while Radicale still serves every real CalDAV operation. This lets the gating
// path be exercised end to end without a heavyweight RFC 6638 server in the matrix.
func autoScheduleProxy(t *testing.T, base string) string {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Method == http.MethodOptions {
			resp.Header.Add("DAV", "calendar-auto-schedule")
		}
		return nil
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return srv.URL
}

// recordingSender notes whether an iMIP reply was emailed.
type recordingSender struct{ sent bool }

func (s *recordingSender) Send(context.Context, string, []string, []byte) error {
	s.sent = true
	return nil
}
func (s *recordingSender) Close() error { return nil }

type e2eSchedulingProvider struct {
	sender *recordingSender
	addr   string
}

func (p e2eSchedulingProvider) Sender(context.Context) (smtp.Sender, error) { return p.sender, nil }
func (p e2eSchedulingProvider) MailboxAddress() string                      { return p.addr }

type clientCalendarProvider struct{ cl *Client }

func (p clientCalendarProvider) Calendar(context.Context) (calendar.Backend, error) { return p.cl, nil }

// TestRadicaleNativeSchedulingGating exercises the RFC 6638 capability switch end
// to end: with the proxy making Radicale look like a native scheduler, POSTing
// accept through the real Graph server must record the responder's PARTSTAT via
// CalDAV (UpdateEvent → Radicale) and NOT email an iMIP reply.
//
// INTERIM: only the "native" signal is faked (one injected OPTIONS header) —
// everything else is real (handler, adapter, CalDAV PUT/GET). Replacing this with
// a real RFC 6638 server (Stalwart, which advertises calendar-auto-schedule and
// actually emits the reply) is tracked separately. The request sends an explicit
// SendResponse:true because an omitted field currently decodes to false (a spec
// default the handler can't override yet — also tracked separately).
func TestRadicaleNativeSchedulingGating(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base := startRadicale(t)
	seedCalendar(t, base)

	// The adapter talks to Radicale through the proxy, so OPTIONS reports
	// calendar-auto-schedule and the adapter detects native scheduling.
	cl, err := Dial(autoScheduleProxy(t, base), radicaleUser, radicalePass, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	if native, err := cl.SupportsServerScheduling(ctx); err != nil || !native {
		t.Fatalf("SupportsServerScheduling = %v, %v; want true (proxy injects calendar-auto-schedule)", native, err)
	}

	cals, err := cl.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	var calID string
	for _, c := range cals {
		if c.Name == calendarName {
			calID = c.ID
		}
	}
	if calID == "" {
		t.Fatalf("calendar %q not found in %+v", calendarName, cals)
	}

	const mailbox = "me@example.com"
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	created, err := cl.CreateEvent(ctx, calID, calendar.Event{
		Subject:   "Planning",
		Start:     start,
		End:       start.Add(time.Hour),
		Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
		Attendees: []calendar.Attendee{{Email: mailbox, Status: "notResponded"}},
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	// Drive the real accept handler over HTTP through the generated Graph server.
	sender := &recordingSender{}
	graphSrv, err := server.New(nil, clientCalendarProvider{cl: cl}, nil, e2eSchedulingProvider{sender: sender, addr: mailbox})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	gateway := httptest.NewServer(graphSrv)
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1.0/me/events/"+created.ID+"/accept", "application/json", bytes.NewReader([]byte(`{"SendResponse": true}`)))
	if err != nil {
		t.Fatalf("POST accept: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("accept status = %d, want 204; body=%s", resp.StatusCode, body)
	}

	// Native path: no email, and the responder's PARTSTAT is now ACCEPTED in Radicale.
	if sender.sent {
		t.Error("emailed an iMIP reply even though the server schedules natively")
	}
	got, err := cl.GetEvent(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetEvent after accept: %v", err)
	}
	var status string
	for _, a := range got.Attendees {
		if a.Email == mailbox {
			status = a.Status
		}
	}
	if status != "accepted" {
		t.Errorf("attendee status after accept = %q, want accepted (attendees: %+v)", status, got.Attendees)
	}
}

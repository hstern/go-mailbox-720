//go:build dockertest

package caldav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/server"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

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

// TestStalwartNativeSchedulingGating exercises the RFC 6638 capability switch end
// to end against a REAL native scheduler (Stalwart). POSTing accept through the
// real Graph server must record the responder's PARTSTAT via CalDAV (UpdateEvent
// → Stalwart, which then performs the iTIP reply itself) and must NOT email an
// iMIP reply from us — the whole point of the switch.
func TestStalwartNativeSchedulingGating(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	base, login, pass := startStalwart(t)

	cl, err := Dial(base, login, pass, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	// The adapter must detect Stalwart as a native scheduler (it advertises
	// calendar-auto-schedule in its OPTIONS DAV header).
	if native, err := cl.SupportsServerScheduling(ctx); err != nil || !native {
		t.Fatalf("SupportsServerScheduling = %v, %v; want true (Stalwart is RFC 6638)", native, err)
	}

	cals, err := cl.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) == 0 {
		t.Fatalf("no calendars discovered for %s", login)
	}
	calID := cals[0].ID

	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	created, err := cl.CreateEvent(ctx, calID, calendar.Event{
		Subject:   "Planning",
		Start:     start,
		End:       start.Add(time.Hour),
		Organizer: calendar.Address{Name: "Alice", Email: "alice@example.com"},
		Attendees: []calendar.Attendee{{Email: login, Status: "notResponded"}},
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	// Drive the real accept handler over HTTP. The empty body relies on
	// SendResponse defaulting to true (the subsetter strips Graph's default:false).
	sender := &recordingSender{}
	graphSrv, err := server.New(nil, clientCalendarProvider{cl: cl}, nil, e2eSchedulingProvider{sender: sender, addr: login})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	gateway := httptest.NewServer(graphSrv)
	defer gateway.Close()

	resp, err := http.Post(gateway.URL+"/v1.0/me/events/"+created.ID+"/accept", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST accept: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("accept status = %d, want 204; body=%s", resp.StatusCode, body)
	}

	// Native path: no email from us, and the responder's PARTSTAT is now ACCEPTED
	// in Stalwart (which performs the iTIP reply itself).
	if sender.sent {
		t.Error("emailed an iMIP reply even though the server schedules natively")
	}
	got, err := cl.GetEvent(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetEvent after accept: %v", err)
	}
	var status string
	for _, a := range got.Attendees {
		if a.Email == login {
			status = a.Status
		}
	}
	if status != "accepted" {
		t.Errorf("attendee status after accept = %q, want accepted (attendees: %+v)", status, got.Attendees)
	}
}

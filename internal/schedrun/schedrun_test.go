package schedrun

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/itip"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
)

type fakeWatcher struct{ fires int }

func (w *fakeWatcher) Watch(ctx context.Context, _ string, onChange func()) error {
	for i := 0; i < w.fires; i++ {
		onChange()
	}
	<-ctx.Done()
	return nil
}

type fakeSyncer struct {
	calls int
	msgs  []mail.Message
}

func (s *fakeSyncer) Delta(_ context.Context, _, _ string) ([]mail.Message, []string, string, error) {
	s.calls++
	if s.calls == 1 {
		return nil, nil, "base", nil // baseline
	}
	return s.msgs, nil, fmt.Sprintf("t%d", s.calls), nil
}

type fakeRaw struct{ byID map[string][]byte }

func (r fakeRaw) RawMessage(_ context.Context, id string) ([]byte, error) {
	b, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("no raw for %s", id)
	}
	return b, nil
}

type fakeWriter struct{ created []calendar.Event }

func (w *fakeWriter) CreateEvent(_ context.Context, calID string, e calendar.Event) (calendar.Event, error) {
	e.ID = fmt.Sprintf("evt-%d", len(w.created))
	e.CalendarID = calID
	w.created = append(w.created, e)
	return e, nil
}
func (w *fakeWriter) UpdateEvent(_ context.Context, e calendar.Event) (calendar.Event, error) {
	return e, nil
}
func (w *fakeWriter) DeleteEvent(_ context.Context, _ string) error { return nil }

func requestMessage(t *testing.T) []byte {
	t.Helper()
	inv := &scheduling.Invite{
		Method:    scheduling.MethodRequest,
		UID:       "uid-1",
		Summary:   "Sync",
		Organizer: scheduling.Address{Name: "Org", Email: "org@example.com"},
		Attendees: []scheduling.Attendee{{Address: scheduling.Address{Email: "me@example.com"}}},
		Start:     time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC),
	}
	ics, err := scheduling.Request(*inv)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	msg, err := scheduling.Compose(inv.Organizer, []scheduling.Address{{Email: "me@example.com"}},
		"Invitation: Sync", scheduling.MethodRequest, ics, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return msg
}

func TestRunCreatesTentativeEventForRequestOnly(t *testing.T) {
	plain := []byte("From: a@example.com\r\nTo: me@example.com\r\nSubject: hi\r\n\r\njust mail\r\n")

	syncer := &fakeSyncer{msgs: []mail.Message{{ID: "req-1"}, {ID: "plain-1"}}}
	raw := fakeRaw{byID: map[string][]byte{"req-1": requestMessage(t), "plain-1": plain}}
	writer := &fakeWriter{}
	watcher := &fakeWatcher{fires: 1}

	ctx, cancel := context.WithCancel(context.Background())
	created := make(chan calendar.Event, 4)
	errs := make(chan error, 4)
	done := make(chan struct{})
	go func() {
		_ = Run(ctx, watcher, syncer, raw, writer, "cal-1", func(e calendar.Event, err error) {
			if err != nil {
				errs <- err
			} else {
				created <- e
			}
		})
		close(done)
	}()

	select {
	case e := <-created:
		if e.Title != "Sync" {
			t.Errorf("title = %q, want Sync", e.Title)
		}
		if e.Status != itip.StatusTentative {
			t.Errorf("status = %q, want %q", e.Status, itip.StatusTentative)
		}
		if e.UID != "uid-1" {
			t.Errorf("uid = %q, want uid-1", e.UID)
		}
		if e.CalendarID != "cal-1" {
			t.Errorf("calendarID = %q, want cal-1", e.CalendarID)
		}
	case err := <-errs:
		t.Fatalf("unexpected error report: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no tentative event created")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if len(writer.created) != 1 {
		t.Errorf("created %d events, want 1 (ordinary mail must be skipped)", len(writer.created))
	}
	select {
	case err := <-errs:
		t.Errorf("ordinary mail produced an error report instead of a silent skip: %v", err)
	default:
	}
}

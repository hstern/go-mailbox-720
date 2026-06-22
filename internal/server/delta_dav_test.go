package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// deltaEvent builds a minimal changed event carrying just a title — enough for the
// delta handler to map it to a Graph event alongside the tombstone.
func deltaEvent(title string) calendar.Event {
	var e calendar.Event
	e.Title = title
	return e
}

type deltaCalendarBackend struct {
	fakeCalendarBackend
	changed []calendar.Event
	removed []string
	next    string
}

func (b *deltaCalendarBackend) Delta(_ context.Context, _, _ string) ([]calendar.Event, []string, string, error) {
	return b.changed, b.removed, b.next, nil
}

type deltaCalendarProvider struct{ backend *deltaCalendarBackend }

func (p deltaCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

// deltaResponse models the wire shape: changed objects and tombstones are both
// generic maps so the test can inspect either.
type deltaResponse struct {
	DeltaLink string           `json:"@odata.deltaLink"`
	Value     []map[string]any `json:"value"`
}

func TestEventsDeltaHandlerEmitsTombstones(t *testing.T) {
	backend := &deltaCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{calendars: []calendar.Calendar{{ID: "cal-1", Name: "Calendar"}}},
		changed:             []calendar.Event{deltaEvent("Standup")},
		removed:             []string{"evt-gone"},
		next:                "synctok",
	}
	h := EventsDeltaHandler(deltaCalendarProvider{backend: backend})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/events/delta()?$deltatoken=old", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp deltaResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if !strings.Contains(resp.DeltaLink, "/me/events/delta()") || !strings.Contains(resp.DeltaLink, "$deltatoken=synctok") {
		t.Errorf("deltaLink = %q", resp.DeltaLink)
	}
	if len(resp.Value) != 2 {
		t.Fatalf("value length = %d, want 2 (one changed + one tombstone): %s", len(resp.Value), w.Body.String())
	}
	if resp.Value[0]["subject"] != "Standup" {
		t.Errorf("changed item = %v, want subject Standup", resp.Value[0])
	}
	tomb := resp.Value[1]
	if tomb["id"] != "evt-gone" {
		t.Errorf("tombstone id = %v, want evt-gone", tomb["id"])
	}
	removed, ok := tomb["@removed"].(map[string]any)
	if !ok || removed["reason"] != "deleted" {
		t.Errorf("tombstone @removed = %v, want {reason: deleted}", tomb["@removed"])
	}
}

func TestEventsDeltaHandlerNotImplemented(t *testing.T) {
	// A backend that is not a DeltaReader yields 501.
	h := EventsDeltaHandler(fakeCalendarProvider{backend: newCalendarFixture()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/events/delta()", nil))
	if w.Code != 501 {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

type deltaContactsBackend struct {
	fakeContactsBackend
	changed []contacts.Contact
	removed []string
	next    string
}

func (b *deltaContactsBackend) Delta(_ context.Context, _, _ string) ([]contacts.Contact, []string, string, error) {
	return b.changed, b.removed, b.next, nil
}

type deltaContactsProvider struct{ backend *deltaContactsBackend }

func (p deltaContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return p.backend, nil
}

func TestContactsDeltaHandlerEmitsTombstones(t *testing.T) {
	backend := &deltaContactsBackend{
		fakeContactsBackend: fakeContactsBackend{books: []contacts.AddressBook{{ID: "book-1", Name: "Contacts"}}},
		changed:             []contacts.Contact{{DisplayName: "Bob"}},
		removed:             []string{"contact-gone"},
		next:                "synctok",
	}
	h := ContactsDeltaHandler(deltaContactsProvider{backend: backend})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/contacts/delta()?$deltatoken=old", nil))

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp deltaResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Value) != 2 {
		t.Fatalf("value length = %d, want 2: %s", len(resp.Value), w.Body.String())
	}
	if resp.Value[0]["displayName"] != "Bob" {
		t.Errorf("changed item = %v, want displayName Bob", resp.Value[0])
	}
	tomb := resp.Value[1]
	if tomb["id"] != "contact-gone" {
		t.Errorf("tombstone id = %v, want contact-gone", tomb["id"])
	}
	removed, ok := tomb["@removed"].(map[string]any)
	if !ok || removed["reason"] != "deleted" {
		t.Errorf("tombstone @removed = %v, want {reason: deleted}", tomb["@removed"])
	}
}

func TestContactsDeltaHandlerNotImplemented(t *testing.T) {
	h := ContactsDeltaHandler(fakeContactsProvider{backend: newContactsFixture()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1.0/me/contacts/delta()", nil))
	if w.Code != 501 {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

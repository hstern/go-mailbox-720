package server

import (
	"context"
	"strings"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

type deltaCalendarBackend struct {
	fakeCalendarBackend
	changed []calendar.Event
	next    string
}

func (b *deltaCalendarBackend) Delta(_ context.Context, _, _ string) ([]calendar.Event, string, error) {
	return b.changed, b.next, nil
}

type deltaCalendarProvider struct{ backend *deltaCalendarBackend }

func (p deltaCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return p.backend, nil
}

func TestMeEventsDelta(t *testing.T) {
	backend := &deltaCalendarBackend{
		fakeCalendarBackend: fakeCalendarBackend{calendars: []calendar.Calendar{{ID: "cal-1", Name: "Calendar"}}},
		changed:             []calendar.Event{{Subject: "Standup"}},
		next:                "synctok",
	}
	h := Handler{calendar: deltaCalendarProvider{backend: backend}}

	res, err := h.MeEventsDelta(context.Background(), api.MeEventsDeltaParams{Deltatoken: api.NewOptString("old")})
	if err != nil {
		t.Fatalf("MeEventsDelta: %v", err)
	}
	ok, isOK := res.(*api.MeEventsDelta2XXStatusCode)
	if !isOK {
		t.Fatalf("response = %T, want *MeEventsDelta2XXStatusCode", res)
	}
	if len(ok.Response.Value) != 1 || ok.Response.Value[0].Subject.Or("") != "Standup" {
		t.Errorf("value = %+v, want one event 'Standup'", ok.Response.Value)
	}
	if link := ok.Response.OdataDotDeltaLink.Or(""); !strings.Contains(link, "/me/events/delta()") || !strings.Contains(link, "$deltatoken=synctok") {
		t.Errorf("deltaLink = %q, want the events delta path + next token", link)
	}
}

func TestMeEventsDeltaReadOnlyNotImplemented(t *testing.T) {
	h := Handler{calendar: fakeCalendarProvider{backend: newCalendarFixture()}} // Backend, no DeltaReader
	if _, err := h.MeEventsDelta(context.Background(), api.MeEventsDeltaParams{}); err == nil {
		t.Error("MeEventsDelta on a non-DeltaReader backend: expected error (501), got nil")
	}
}

type deltaContactsBackend struct {
	fakeContactsBackend
	changed []contacts.Contact
	next    string
}

func (b *deltaContactsBackend) Delta(_ context.Context, _, _ string) ([]contacts.Contact, string, error) {
	return b.changed, b.next, nil
}

type deltaContactsProvider struct{ backend *deltaContactsBackend }

func (p deltaContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return p.backend, nil
}

func TestMeContactsDelta(t *testing.T) {
	backend := &deltaContactsBackend{
		fakeContactsBackend: fakeContactsBackend{books: []contacts.AddressBook{{ID: "book-1", Name: "Contacts"}}},
		changed:             []contacts.Contact{{DisplayName: "Bob"}},
		next:                "synctok",
	}
	h := Handler{contacts: deltaContactsProvider{backend: backend}}

	res, err := h.MeContactsDelta(context.Background(), api.MeContactsDeltaParams{Deltatoken: api.NewOptString("old")})
	if err != nil {
		t.Fatalf("MeContactsDelta: %v", err)
	}
	ok, isOK := res.(*api.MeContactsDelta2XXStatusCode)
	if !isOK {
		t.Fatalf("response = %T, want *MeContactsDelta2XXStatusCode", res)
	}
	if len(ok.Response.Value) != 1 || ok.Response.Value[0].DisplayName.Or("") != "Bob" {
		t.Errorf("value = %+v, want one contact 'Bob'", ok.Response.Value)
	}
	if link := ok.Response.OdataDotDeltaLink.Or(""); !strings.Contains(link, "/me/contacts/delta()") || !strings.Contains(link, "$deltatoken=synctok") {
		t.Errorf("deltaLink = %q, want the contacts delta path + next token", link)
	}
}

func TestMeContactsDeltaReadOnlyNotImplemented(t *testing.T) {
	h := Handler{contacts: fakeContactsProvider{backend: newContactsFixture()}} // Backend, no DeltaReader
	if _, err := h.MeContactsDelta(context.Background(), api.MeContactsDeltaParams{}); err == nil {
		t.Error("MeContactsDelta on a non-DeltaReader backend: expected error (501), got nil")
	}
}

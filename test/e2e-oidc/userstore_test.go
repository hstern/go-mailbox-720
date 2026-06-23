// userstore_test.go provides a shared in-process per-subject store used by the
// impersonation e2e to seed and retrieve per-user data across backend stubs.
// It is also the home for TestUserStoreIsolatesSubjects, a pure unit test (no
// docker) that verifies subject isolation before any backend is wired.
package e2e

import (
	"sync"
	"testing"
)

// message is a seeded IMAP message stub.
type message struct {
	Subject  string
	FromAddr string
}

// event is a seeded CalDAV event stub. Organizer, when non-empty, is emitted as
// the VEVENT ORGANIZER so accept/decline produces an iMIP REPLY to that address.
type event struct {
	Subject   string
	Organizer string
}

// contact is a seeded CardDAV contact stub.
type contact struct {
	DisplayName string
}

// sentMail records an SMTP delivery that a stub backend captured.
type sentMail struct {
	From string
	To   []string
	Data string
}

// userStore holds per-subject seed data and SMTP captures, guarded by a mutex
// so concurrent backend stubs may call it safely.
type userStore struct {
	mu       sync.Mutex
	msgs     map[string][]message
	evts     map[string][]event
	cts      map[string][]contact
	outbox   map[string][]sentMail
}

// newUserStore returns an empty, ready-to-use store.
func newUserStore() *userStore {
	return &userStore{
		msgs:   make(map[string][]message),
		evts:   make(map[string][]event),
		cts:    make(map[string][]contact),
		outbox: make(map[string][]sentMail),
	}
}

// seedMessages appends messages for sub.
func (s *userStore) seedMessages(sub string, m ...message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs[sub] = append(s.msgs[sub], m...)
}

// seedEvents appends events for sub.
func (s *userStore) seedEvents(sub string, e ...event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evts[sub] = append(s.evts[sub], e...)
}

// seedContacts appends contacts for sub.
func (s *userStore) seedContacts(sub string, c ...contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cts[sub] = append(s.cts[sub], c...)
}

// messages returns the messages seeded for sub (empty slice for unknown subs).
func (s *userStore) messages(sub string) []message {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.msgs[sub]
	if v == nil {
		return []message{}
	}
	return v
}

// events returns the events seeded for sub (empty slice for unknown subs).
func (s *userStore) events(sub string) []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.evts[sub]
	if v == nil {
		return []event{}
	}
	return v
}

// contacts returns the contacts seeded for sub (empty slice for unknown subs).
func (s *userStore) contacts(sub string) []contact {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.cts[sub]
	if v == nil {
		return []contact{}
	}
	return v
}

// recordSent appends a sent mail record for sub.
func (s *userStore) recordSent(sub string, m sentMail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outbox[sub] = append(s.outbox[sub], m)
}

// sent returns the sent mail records for sub (empty slice for unknown subs).
func (s *userStore) sent(sub string) []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.outbox[sub]
	if v == nil {
		return []sentMail{}
	}
	return v
}

// TestUserStoreIsolatesSubjects verifies that seedMessages and messages keep
// per-subject data isolated and that unknown subs return empty (never panic).
// This test requires no docker; it runs in any environment.
func TestUserStoreIsolatesSubjects(t *testing.T) {
	s := newUserStore()
	s.seedMessages("subA", message{Subject: "A-only", FromAddr: "a@example.com"})
	s.seedMessages("subB", message{Subject: "B-only", FromAddr: "b@example.com"})
	if got := s.messages("subA"); len(got) != 1 || got[0].Subject != "A-only" {
		t.Fatalf("subA messages = %+v", got)
	}
	if got := s.messages("subB"); len(got) != 1 || got[0].Subject != "B-only" {
		t.Fatalf("subB messages = %+v", got)
	}
	if got := s.messages("subC"); len(got) != 0 {
		t.Fatalf("unknown sub returned data: %+v", got)
	}
}

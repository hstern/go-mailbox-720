package subscriptions

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Store-level sentinel errors.
var (
	// ErrNotFound is returned by Get and Delete when no subscription has the id.
	ErrNotFound = errors.New("subscriptions: subscription not found")
	// ErrDuplicateID is returned by Create when a subscription's id already exists.
	ErrDuplicateID = errors.New("subscriptions: duplicate subscription id")
)

// Store persists subscriptions. Implementations must be safe for concurrent use.
// A future POST /subscriptions handler creates through it, a renewal PATCH reads
// and rewrites, and the delivery loop lists active subscriptions.
type Store interface {
	// Create persists sub. It stamps CreatedAt, assigns an ID when sub.ID is
	// empty, and returns the stored subscription. It returns ErrDuplicateID if
	// sub.ID is set and already in use.
	Create(sub Subscription) (Subscription, error)
	// Get returns the subscription with the given id, or ErrNotFound.
	Get(id string) (Subscription, error)
	// List returns all stored subscriptions, ordered by CreatedAt.
	List() []Subscription
	// Delete removes the subscription with the given id, or returns ErrNotFound.
	Delete(id string) error
	// DeleteExpired removes every subscription whose ExpirationDateTime is at or
	// before now and returns how many were removed.
	DeleteExpired(now time.Time) int
}

// MemoryStore is an in-memory, concurrency-safe [Store]. It guards its map with a
// mutex; it is suitable for a single process and resets on restart.
type MemoryStore struct {
	mu   sync.Mutex
	subs map[string]Subscription
	// now is the clock used to stamp CreatedAt, overridable in tests.
	now func() time.Time
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		subs: make(map[string]Subscription),
		now:  time.Now,
	}
}

// Create stamps CreatedAt, assigns a crypto/rand id when sub.ID is empty, and
// stores the subscription. A caller-provided id that already exists is rejected
// with ErrDuplicateID.
func (s *MemoryStore) Create(sub Subscription) (Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sub.ID == "" {
		id, err := s.uniqueID()
		if err != nil {
			return Subscription{}, err
		}
		sub.ID = id
	} else if _, exists := s.subs[sub.ID]; exists {
		return Subscription{}, fmt.Errorf("%w: %q", ErrDuplicateID, sub.ID)
	}

	sub.CreatedAt = s.now().UTC()
	s.subs[sub.ID] = sub
	return sub, nil
}

// uniqueID draws a random id not already present. The caller must hold s.mu.
func (s *MemoryStore) uniqueID() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		id, err := randomToken()
		if err != nil {
			return "", fmt.Errorf("subscriptions: generate id: %w", err)
		}
		if _, exists := s.subs[id]; !exists {
			return id, nil
		}
	}
	return "", errors.New("subscriptions: could not generate a unique id")
}

// Get returns the subscription with id, or ErrNotFound.
func (s *MemoryStore) Get(id string) (Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sub, ok := s.subs[id]
	if !ok {
		return Subscription{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return sub, nil
}

// List returns all subscriptions ordered by CreatedAt (ties broken by ID) so the
// result is deterministic.
func (s *MemoryStore) List() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Delete removes the subscription with id, or returns ErrNotFound.
func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.subs[id]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	delete(s.subs, id)
	return nil
}

// DeleteExpired removes every subscription whose ExpirationDateTime is at or
// before now, returning the number removed.
func (s *MemoryStore) DeleteExpired(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, sub := range s.subs {
		if !sub.ExpirationDateTime.After(now) {
			delete(s.subs, id)
			removed++
		}
	}
	return removed
}

// MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)

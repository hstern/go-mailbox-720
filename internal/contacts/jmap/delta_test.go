package jmap

import (
	"context"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

func TestDeltaReturnsChangedCardsInAddressBook(t *testing.T) {
	f := newFixture()
	f.changesNewState = "s1"
	f.changesCreated = []gojmap.ID{"card-1"} // card-1 is in ab-1 (see aliceCard)

	cl := f.start(t)
	changed, removed, next, err := cl.Delta(context.Background(), "ab-1", "old")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if next != "s1" {
		t.Errorf("next = %q, want s1", next)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	if len(changed) != 1 || changed[0].ID != "card-1" || changed[0].AddressBookID != "ab-1" {
		t.Fatalf("changed = %+v, want one card-1 in ab-1", changed)
	}
}

func TestDeltaFiltersOtherAddressBooks(t *testing.T) {
	f := newFixture()
	f.changesNewState = "s1"
	f.changesCreated = []gojmap.ID{"card-1"} // card-1 belongs to ab-1, not ab-2

	cl := f.start(t)
	changed, _, next, err := cl.Delta(context.Background(), "ab-2", "old")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if next != "s1" {
		t.Errorf("next = %q, want s1", next)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %+v, want none (card-1 is not in ab-2)", changed)
	}
}

func TestDeltaReportsDestroyedAsRemoved(t *testing.T) {
	f := newFixture()
	f.changesNewState = "s2"
	f.changesDestroyed = []gojmap.ID{"card-gone"}

	cl := f.start(t)
	changed, removed, next, err := cl.Delta(context.Background(), "ab-1", "old")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if next != "s2" {
		t.Errorf("next = %q, want s2", next)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %+v, want none", changed)
	}
	if len(removed) != 1 || removed[0] != "card-gone" {
		t.Fatalf("removed = %v, want [card-gone]", removed)
	}
}

func TestDeltaEmptyWhenNoChanges(t *testing.T) {
	f := newFixture()
	f.changesNewState = "s0"

	cl := f.start(t)
	changed, removed, next, err := cl.Delta(context.Background(), "ab-1", "s0")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(changed) != 0 || len(removed) != 0 {
		t.Fatalf("changed=%+v removed=%v, want both empty", changed, removed)
	}
	if next != "s0" {
		t.Errorf("next = %q, want s0", next)
	}
}

package jmap

import (
	"encoding/json"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

func TestEventGetMethodShape(t *testing.T) {
	m := &eventGet{Account: "acc1", IDs: []gojmap.ID{"e1"}}
	if m.Name() != "CalendarEvent/get" {
		t.Fatalf("Name = %q", m.Name())
	}
	if got := m.Requires(); len(got) != 1 || got[0] != calendarsURI {
		t.Fatalf("Requires = %v", got)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"accountId":"acc1","ids":["e1"]}` {
		t.Fatalf("json = %s", b)
	}
}

func TestEventQueryExpandRecurrencesMarshals(t *testing.T) {
	m := &eventQuery{Account: "acc1", Filter: &eventFilter{InCalendars: []gojmap.ID{"c1"}, After: "2026-01-01T00:00:00Z", Before: "2026-02-01T00:00:00Z"}, ExpandRecurrences: true}
	b, _ := json.Marshal(m)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["expandRecurrences"] != true {
		t.Fatalf("expandRecurrences missing: %s", b)
	}
}

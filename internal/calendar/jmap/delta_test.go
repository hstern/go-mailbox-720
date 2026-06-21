package jmap

import (
	"context"
	"net/http"
	"testing"
)

func TestDelta(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		switch methodName(body) {
		case "CalendarEvent/changes":
			respond(w, "CalendarEvent/changes", map[string]any{
				"accountId": "acc1",
				"oldState":  "1",
				"newState":  "2",
				"created":   []string{"e1"},
				"updated":   []string{},
				"destroyed": []string{"e0"},
			})
		case "CalendarEvent/get":
			respond(w, "CalendarEvent/get", map[string]any{
				"accountId": "acc1", "state": "2",
				"list": []map[string]any{
					{
						"id":          "e1",
						"uid":         "u1",
						"title":       "New Event",
						"calendarIds": map[string]bool{"c1": true},
						"utcStart":    "2026-06-21T10:00:00Z",
						"utcEnd":      "2026-06-21T11:00:00Z",
					},
				},
				"notFound": []string{},
			})
		default:
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
		}
	})

	changed, removed, next, err := cl.Delta(context.Background(), "c1", "1")
	if err != nil {
		t.Fatalf("Delta: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("want 1 changed event, got %d: %+v", len(changed), changed)
	}
	if changed[0].ID != "e1" {
		t.Errorf("changed[0].ID = %q, want e1", changed[0].ID)
	}
	if len(removed) != 1 || removed[0] != "e0" {
		t.Errorf("removed = %v, want [e0]", removed)
	}
	if next != "2" {
		t.Errorf("next = %q, want 2", next)
	}
}

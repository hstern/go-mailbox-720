package jmap

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func TestCreateEvent(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "2",
			"created": map[string]any{
				"new": map[string]any{
					"id":  "e9",
					"uid": "u9",
				},
			},
			"notCreated":   nil,
			"notUpdated":   nil,
			"notDestroyed": nil,
		})
	})

	e := calendar.Event{
		Subject: "New Event",
		UID:     "u9",
	}
	created, err := cl.CreateEvent(context.Background(), "cal1", e)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if created.ID != "e9" {
		t.Errorf("ID = %q, want e9", created.ID)
	}
}

func TestCreateEventNilCreated(t *testing.T) {
	// Server returns created with a null/nil value for "new" — we fall back to
	// the event we sent, stamped with the ID from the response key.
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		// Some servers return {"new": null} in created.
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "2",
			"created": map[string]any{
				"new": nil,
			},
			"notCreated":   nil,
			"notUpdated":   nil,
			"notDestroyed": nil,
		})
	})

	e := calendar.Event{
		Subject: "New Event",
		UID:     "u9",
	}
	// With a nil created response we fall back to the sent event; we cannot
	// return a server-assigned ID because the server returned null. The call
	// should not error, and the returned ID must be empty (known limitation:
	// callers needing the server-assigned id must re-fetch).
	got, err := cl.CreateEvent(context.Background(), "cal1", e)
	if err != nil {
		t.Fatalf("CreateEvent (nil created): %v", err)
	}
	if got.ID != "" {
		t.Errorf("ID = %q, want empty (server-assigned id not recoverable from null created)", got.ID)
	}
}

func TestCreateEventRejected(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "1",
			"notCreated": map[string]any{
				"new": map[string]any{
					"type": "invalidProperties",
				},
			},
		})
	})

	e := calendar.Event{Subject: "Bad Event"}
	_, err := cl.CreateEvent(context.Background(), "cal1", e)
	if err == nil {
		t.Fatal("CreateEvent: expected error for notCreated, got nil")
	}
}

func TestUpdateEvent(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "2",
			"updated": map[string]any{
				"e1": nil,
			},
			"notUpdated":   nil,
			"notDestroyed": nil,
		})
	})

	e := calendar.Event{
		ID:      "e1",
		Subject: "Updated Event",
		UID:     "u1",
	}
	updated, err := cl.UpdateEvent(context.Background(), e)
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if updated.ID != "e1" {
		t.Errorf("ID = %q, want e1", updated.ID)
	}
}

func TestUpdateEventRejected(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "1",
			"notUpdated": map[string]any{
				"e1": map[string]any{
					"type":        "alreadyExists",
					"description": "conflict",
				},
			},
		})
	})

	e := calendar.Event{ID: "e1", Subject: "Conflict Event"}
	_, err := cl.UpdateEvent(context.Background(), e)
	if err == nil {
		t.Fatal("UpdateEvent: expected error for notUpdated, got nil")
	}
}

func TestDeleteEvent(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId":    "acc1",
			"oldState":     "1",
			"newState":     "2",
			"destroyed":    []string{"e1"},
			"notDestroyed": nil,
		})
	})

	err := cl.DeleteEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
}

func TestDeleteEventRejected(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "1",
			"notDestroyed": map[string]any{
				"e1": map[string]any{
					"type":        "notFound",
					"description": "no such event",
				},
			},
		})
	})

	err := cl.DeleteEvent(context.Background(), "e1")
	if err == nil {
		t.Fatal("DeleteEvent: expected error for notDestroyed, got nil")
	}
}

// TestWriteInstanceOverride verifies that WriteInstanceOverride sends a
// CalendarEvent/set Update for the synthetic instance id, with
// sendSchedulingMessages:true, and stamps the returned event IsOverride=true.
func TestWriteInstanceOverride(t *testing.T) {
	var capturedUpdate map[string]any
	var capturedScheduling bool

	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/set" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		// Capture the request args for later assertion.
		calls, _ := body["methodCalls"].([]any)
		if len(calls) > 0 {
			call, _ := calls[0].([]any)
			if len(call) > 1 {
				args, _ := call[1].(map[string]any)
				capturedUpdate, _ = args["update"].(map[string]any)
				capturedScheduling, _ = args["sendSchedulingMessages"].(bool)
			}
		}
		respond(w, "CalendarEvent/set", map[string]any{
			"accountId": "acc1",
			"oldState":  "1",
			"newState":  "2",
			"updated": map[string]any{
				"e1_i0": map[string]any{
					"id":    "e1_i0",
					"title": "Override Title",
				},
			},
			"notUpdated":   nil,
			"notDestroyed": nil,
		})
	})

	rid := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	override := calendar.Event{
		ID:             "e1_i0",
		Subject:        "Override Title",
		RecurrenceID:   rid,
		SeriesMasterID: "e1",
	}
	got, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err != nil {
		t.Fatalf("WriteInstanceOverride: %v", err)
	}
	if !got.IsOverride {
		t.Errorf("IsOverride = false, want true")
	}
	// Verify sendSchedulingMessages:true was sent.
	if !capturedScheduling {
		t.Errorf("sendSchedulingMessages not set to true in request")
	}
	// Verify the update was keyed by the synthetic instance id.
	if capturedUpdate == nil {
		t.Fatal("update map was nil in request")
	}
	if _, ok := capturedUpdate["e1_i0"]; !ok {
		t.Errorf("update map missing key e1_i0; got keys: %v", capturedUpdate)
	}
}

// TestWriteInstanceOverrideNoRecurrenceID verifies that WriteInstanceOverride
// returns an error (and makes no HTTP call) when override.RecurrenceID is zero.
func TestWriteInstanceOverrideNoRecurrenceID(t *testing.T) {
	called := false
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		called = true
		http.Error(w, "unexpected call", http.StatusBadRequest)
	})

	override := calendar.Event{
		ID:      "e1_i0",
		Subject: "Should Not Send",
		// RecurrenceID deliberately zero
	}
	_, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err == nil {
		t.Fatal("expected error for zero RecurrenceID, got nil")
	}
	if called {
		t.Error("HTTP handler was called despite zero RecurrenceID — expected early return")
	}
}

// TestWriteInstanceOverrideNoID verifies that WriteInstanceOverride returns an
// error when the caller has not provided the synthetic instance id in override.ID.
func TestWriteInstanceOverrideNoID(t *testing.T) {
	called := false
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		called = true
		http.Error(w, "unexpected call", http.StatusBadRequest)
	})

	rid := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	override := calendar.Event{
		// ID deliberately empty
		Subject:      "No ID Event",
		RecurrenceID: rid,
	}
	_, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err == nil {
		t.Fatal("expected error for empty override.ID, got nil")
	}
	if called {
		t.Error("HTTP handler was called despite empty override.ID — expected early return")
	}
}

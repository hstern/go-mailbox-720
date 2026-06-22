package jmap

import (
	"context"
	"net/http"
	"testing"

	"github.com/hstern/go-jscalendar"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// titledEvent builds a calendar.Event with the given Title and UID set on its
// embedded JSCalendar Event, plus the opaque store ID.
func titledEvent(id, uid, title string) calendar.Event {
	e := calendar.Event{ID: id}
	e.UID = uid
	e.Title = title
	return e
}

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

	e := titledEvent("", "u9", "New Event")
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

	e := titledEvent("", "u9", "New Event")
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

	e := titledEvent("", "", "Bad Event")
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

	e := titledEvent("e1", "u1", "Updated Event")
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

	e := titledEvent("e1", "", "Conflict Event")
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

	override := calendar.Event{ID: "e1_i0", SeriesMasterID: "e1"}
	override.Title = "Override Title"
	override.RecurrenceID = &jscalendar.LocalDateTime{Year: 2024, Month: 3, Day: 15, Hour: 10}
	override.RecurrenceIDTimeZone = "Etc/UTC"
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

	override := calendar.Event{ID: "e1_i0"}
	override.Title = "Should Not Send"
	// RecurrenceID deliberately nil
	_, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err == nil {
		t.Fatal("expected error for nil RecurrenceID, got nil")
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

	override := calendar.Event{} // ID deliberately empty
	override.Title = "No ID Event"
	override.RecurrenceID = &jscalendar.LocalDateTime{Year: 2024, Month: 3, Day: 15, Hour: 10}
	override.RecurrenceIDTimeZone = "Etc/UTC"
	_, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err == nil {
		t.Fatal("expected error for empty override.ID, got nil")
	}
	if called {
		t.Error("HTTP handler was called despite empty override.ID — expected early return")
	}
}

// TestWriteInstanceOverrideNilEcho verifies the nil-echo fallback path in
// WriteInstanceOverride: when the server returns "updated":{"e1_i0": null}
// (RFC 8620 §5.3 permits a null entry), the adapter returns the input override
// with IsOverride stamped true and the original Subject intact.
func TestWriteInstanceOverrideNilEcho(t *testing.T) {
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
				"e1_i0": nil,
			},
			"notUpdated":   nil,
			"notDestroyed": nil,
		})
	})

	override := calendar.Event{ID: "e1_i0"}
	override.Title = "Override Nil Echo"
	override.RecurrenceID = &jscalendar.LocalDateTime{Year: 2024, Month: 3, Day: 15, Hour: 10}
	override.RecurrenceIDTimeZone = "Etc/UTC"
	got, err := cl.WriteInstanceOverride(context.Background(), "e1", override)
	if err != nil {
		t.Fatalf("WriteInstanceOverride (nil echo): %v", err)
	}
	if !got.IsOverride {
		t.Errorf("IsOverride = false, want true (fallback must stamp IsOverride)")
	}
	if got.Title != override.Title {
		t.Errorf("Title = %q, want %q (fallback must return input override)", got.Title, override.Title)
	}
}

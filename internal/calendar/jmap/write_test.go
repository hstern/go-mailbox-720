package jmap

import (
	"context"
	"net/http"
	"testing"

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
	// should not error.
	_, err := cl.CreateEvent(context.Background(), "cal1", e)
	if err != nil {
		t.Fatalf("CreateEvent (nil created): %v", err)
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

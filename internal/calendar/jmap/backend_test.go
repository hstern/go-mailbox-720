package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// dialTest returns a Client wired to a jmapServer with the given POST handler.
func dialTest(t *testing.T, api func(w http.ResponseWriter, body map[string]any)) *Client {
	t.Helper()
	srv := jmapServer(t, api)
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cl
}

// respond writes a JMAP method-response envelope wrapping one invocation.
func respond(w http.ResponseWriter, method string, args any) {
	a, err := json.Marshal(args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"methodResponses": []any{[]any{method, json.RawMessage(a), "c0"}},
		"sessionState":    "s",
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestListCalendars(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, _ map[string]any) {
		respond(w, "Calendar/get", map[string]any{
			"accountId": "acc1", "state": "1",
			"list": []map[string]any{{"id": "c1", "name": "Personal", "description": "mine"}},
		})
	})
	cals, err := cl.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].ID != "c1" || cals[0].Name != "Personal" || cals[0].Description != "mine" {
		t.Fatalf("cals = %+v", cals)
	}
	_ = gojmap.ID("")
}

// methodName extracts the first method call name from a JMAP request body,
// e.g. "CalendarEvent/query" or "CalendarEvent/get".
func methodName(body map[string]any) string {
	calls, _ := body["methodCalls"].([]any)
	if len(calls) == 0 {
		return ""
	}
	call, _ := calls[0].([]any)
	if len(call) == 0 {
		return ""
	}
	name, _ := call[0].(string)
	return name
}

func TestListEvents(t *testing.T) {
	var capturedFilter map[string]any

	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		switch methodName(body) {
		case "CalendarEvent/query":
			// Capture the filter for later assertion.
			calls, _ := body["methodCalls"].([]any)
			call, _ := calls[0].([]any)
			args, _ := call[1].(map[string]any)
			capturedFilter, _ = args["filter"].(map[string]any)
			respond(w, "CalendarEvent/query", map[string]any{
				"accountId": "acc1",
				"ids":       []string{"e1"},
			})
		case "CalendarEvent/get":
			respond(w, "CalendarEvent/get", map[string]any{
				"accountId": "acc1", "state": "1",
				"list": []map[string]any{
					{
						"id":       "e1",
						"uid":      "u1",
						"title":    "M",
						"utcStart": "2026-06-01T10:00:00Z",
						"utcEnd":   "2026-06-01T11:00:00Z",
					},
				},
				"notFound": []string{},
			})
		default:
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
		}
	})

	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	events, err := cl.ListEvents(context.Background(), "cal1", calendar.Range{Start: start, End: end})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(events), events)
	}
	if events[0].ID != "e1" {
		t.Errorf("ID = %q, want e1", events[0].ID)
	}
	if events[0].Title != "M" {
		t.Errorf("Title = %q, want M", events[0].Title)
	}

	// Assert the query carried the calendarID filter and time bounds.
	if capturedFilter == nil {
		t.Fatal("query filter was nil")
	}
	inCals, _ := capturedFilter["inCalendars"].([]any)
	if len(inCals) != 1 || inCals[0] != "cal1" {
		t.Errorf("inCalendars = %v, want [cal1]", inCals)
	}
	if capturedFilter["after"] != start.Format(time.RFC3339) {
		t.Errorf("after = %v, want %v", capturedFilter["after"], start.Format(time.RFC3339))
	}
	if capturedFilter["before"] != end.Format(time.RFC3339) {
		t.Errorf("before = %v, want %v", capturedFilter["before"], end.Format(time.RFC3339))
	}
}

func TestListEventsEmpty(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		// Only the query call should be made; get should NOT fire when ids is empty.
		if methodName(body) != "CalendarEvent/query" {
			http.Error(w, "unexpected method after empty query: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/query", map[string]any{
			"accountId": "acc1",
			"ids":       []string{},
		})
	})
	events, err := cl.ListEvents(context.Background(), "cal1", calendar.Range{})
	if err != nil {
		t.Fatalf("ListEvents empty: %v", err)
	}
	if events != nil {
		t.Errorf("expected nil, got %v", events)
	}
}

func TestGetEvent(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/get" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/get", map[string]any{
			"accountId": "acc1", "state": "1",
			"list": []map[string]any{
				{
					"id":       "e1",
					"uid":      "u1",
					"title":    "Event One",
					"utcStart": "2026-06-15T10:00:00Z",
					"utcEnd":   "2026-06-15T11:00:00Z",
				},
			},
			"notFound": []string{},
		})
	})
	event, err := cl.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if event.ID != "e1" {
		t.Errorf("ID = %q, want e1", event.ID)
	}
	if event.Title != "Event One" {
		t.Errorf("Title = %q, want Event One", event.Title)
	}
}

func TestGetEventNotFound(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		if methodName(body) != "CalendarEvent/get" {
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/get", map[string]any{
			"accountId": "acc1", "state": "1",
			"list":     []map[string]any{},
			"notFound": []string{"e1"},
		})
	})
	_, err := cl.GetEvent(context.Background(), "e1")
	if err == nil {
		t.Fatalf("GetEvent: expected error for non-existent event")
	}
}

func TestFindEventByUIDFound(t *testing.T) {
	var capturedFilter map[string]any

	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		switch methodName(body) {
		case "CalendarEvent/query":
			// Capture the filter for later assertion.
			calls, _ := body["methodCalls"].([]any)
			call, _ := calls[0].([]any)
			args, _ := call[1].(map[string]any)
			capturedFilter, _ = args["filter"].(map[string]any)
			respond(w, "CalendarEvent/query", map[string]any{
				"accountId": "acc1",
				"ids":       []string{"e1"},
			})
		case "CalendarEvent/get":
			respond(w, "CalendarEvent/get", map[string]any{
				"accountId": "acc1", "state": "1",
				"list": []map[string]any{
					{
						"id":       "e1",
						"uid":      "uid-from-server",
						"title":    "Event by UID",
						"utcStart": "2026-06-15T10:00:00Z",
						"utcEnd":   "2026-06-15T11:00:00Z",
					},
				},
				"notFound": []string{},
			})
		default:
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
		}
	})

	event, found, err := cl.FindEventByUID(context.Background(), "cal1", "uid-from-server")
	if err != nil {
		t.Fatalf("FindEventByUID: %v", err)
	}
	if !found {
		t.Errorf("found = %v, want true", found)
	}
	if event.ID != "e1" {
		t.Errorf("ID = %q, want e1", event.ID)
	}
	if event.Title != "Event by UID" {
		t.Errorf("Title = %q, want Event by UID", event.Title)
	}

	// Assert the query carried the uid and calendarID filter.
	if capturedFilter == nil {
		t.Fatal("query filter was nil")
	}
	inCals, _ := capturedFilter["inCalendars"].([]any)
	if len(inCals) != 1 || inCals[0] != "cal1" {
		t.Errorf("inCalendars = %v, want [cal1]", inCals)
	}
	if capturedFilter["uid"] != "uid-from-server" {
		t.Errorf("uid = %v, want uid-from-server", capturedFilter["uid"])
	}
}

func TestFindEventByUIDMissing(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		// Only the query call should be made; get should NOT fire when ids is empty.
		if methodName(body) != "CalendarEvent/query" {
			http.Error(w, "unexpected method after empty query: "+methodName(body), http.StatusBadRequest)
			return
		}
		respond(w, "CalendarEvent/query", map[string]any{
			"accountId": "acc1",
			"ids":       []string{},
		})
	})

	event, found, err := cl.FindEventByUID(context.Background(), "cal1", "nonexistent-uid")
	if err != nil {
		t.Fatalf("FindEventByUID: %v", err)
	}
	if found {
		t.Errorf("found = %v, want false", found)
	}
	if event.ID != "" {
		t.Errorf("event.ID = %q, want empty", event.ID)
	}
}

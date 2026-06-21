package jmap

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func TestListInstancesUnbounded(t *testing.T) {
	// A zero Range must be rejected before any HTTP call is made.
	httpCalled := false
	cl := dialTest(t, func(w http.ResponseWriter, _ map[string]any) {
		httpCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})

	_, err := cl.ListInstances(context.Background(), "e1", calendar.Range{})
	if err == nil {
		t.Fatal("ListInstances with zero range: expected error, got nil")
	}
	if httpCalled {
		t.Error("ListInstances with zero range must not make any HTTP call")
	}
}

func TestListInstances(t *testing.T) {
	// Bounds for the query.
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	r := calendar.Range{Start: start, End: end}

	var capturedQueryArgs map[string]any

	cl := dialTest(t, func(w http.ResponseWriter, body map[string]any) {
		switch methodName(body) {
		case "CalendarEvent/query":
			// Capture the full args for assertion.
			calls, _ := body["methodCalls"].([]any)
			call, _ := calls[0].([]any)
			capturedQueryArgs, _ = call[1].(map[string]any)

			// Return two instance IDs for series "e1" plus one for "e2" to test filtering.
			respond(w, "CalendarEvent/query", map[string]any{
				"accountId": "acc1",
				"ids":       []string{"e1_i0", "e1_i1", "e2_i0"},
			})

		case "CalendarEvent/get":
			// Return two e1 instances and one e2 instance.
			respond(w, "CalendarEvent/get", map[string]any{
				"accountId": "acc1",
				"state":     "1",
				"list": []map[string]any{
					{
						"id":          "e1_i0",
						"uid":         "u1",
						"baseEventId": "e1",
						"recurrenceId": "2026-06-07T10:00:00",
						"utcStart":    "2026-06-07T10:00:00Z",
						"utcEnd":      "2026-06-07T11:00:00Z",
					},
					{
						"id":          "e1_i1",
						"uid":         "u1",
						"baseEventId": "e1",
						"recurrenceId": "2026-06-14T10:00:00",
						"utcStart":    "2026-06-14T10:00:00Z",
						"utcEnd":      "2026-06-14T11:00:00Z",
					},
					{
						"id":          "e2_i0",
						"uid":         "u2",
						"baseEventId": "e2",
						"recurrenceId": "2026-06-07T12:00:00",
						"utcStart":    "2026-06-07T12:00:00Z",
						"utcEnd":      "2026-06-07T13:00:00Z",
					},
				},
				"notFound": []string{},
			})

		default:
			http.Error(w, "unexpected method: "+methodName(body), http.StatusBadRequest)
		}
	})

	instances, err := cl.ListInstances(context.Background(), "e1", r)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}

	// Must return exactly 2 instances (e2_i0 filtered out).
	if len(instances) != 2 {
		t.Fatalf("want 2 instances, got %d: %+v", len(instances), instances)
	}

	// Each instance must carry SeriesMasterID == "e1" and a non-zero RecurrenceID.
	for _, inst := range instances {
		if inst.SeriesMasterID != "e1" {
			t.Errorf("instance %q: SeriesMasterID = %q, want e1", inst.ID, inst.SeriesMasterID)
		}
		if inst.RecurrenceID.IsZero() {
			t.Errorf("instance %q: RecurrenceID is zero", inst.ID)
		}
	}

	// The query must have carried expandRecurrences:true and time bounds.
	if capturedQueryArgs == nil {
		t.Fatal("CalendarEvent/query was not called")
	}
	if capturedQueryArgs["expandRecurrences"] != true {
		t.Errorf("expandRecurrences = %v, want true", capturedQueryArgs["expandRecurrences"])
	}
	filter, _ := capturedQueryArgs["filter"].(map[string]any)
	if filter == nil {
		t.Fatal("query filter was nil")
	}
	if filter["after"] != start.UTC().Format(time.RFC3339) {
		t.Errorf("after = %v, want %v", filter["after"], start.UTC().Format(time.RFC3339))
	}
	if filter["before"] != end.UTC().Format(time.RFC3339) {
		t.Errorf("before = %v, want %v", filter["before"], end.UTC().Format(time.RFC3339))
	}
}

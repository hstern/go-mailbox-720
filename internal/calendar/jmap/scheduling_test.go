package jmap

import (
	"context"
	"net/http"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func TestSupportsServerScheduling(t *testing.T) {
	// dialTest creates a client with a session that advertises the calendars capability.
	cl := dialTest(t, func(w http.ResponseWriter, _ map[string]any) {
		// No request is expected; SupportsServerScheduling should check the session
		// that was already populated during Dial.
	})

	supports, err := cl.SupportsServerScheduling(context.Background())
	if err != nil {
		t.Fatalf("SupportsServerScheduling: %v", err)
	}
	if !supports {
		t.Errorf("supports = %v, want true", supports)
	}
}

// Verify that Client implements the SchedulingDetector interface.
var _ calendar.SchedulingDetector = (*Client)(nil)

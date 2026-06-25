package caldav

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// TestUpdateEventIfMatch exercises the conditional-PUT path: the adapter sends
// the supplied ETag as an If-Match header, returns the event on success, and
// translates the server's 412 into calendar.ErrPreconditionFailed.
func TestUpdateEventIfMatch(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantErr    error
		wantHeader string
	}{
		{name: "match", status: http.StatusNoContent, wantHeader: `"good-etag"`},
		{name: "conflict", status: http.StatusPreconditionFailed, wantErr: calendar.ErrPreconditionFailed, wantHeader: `"good-etag"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotIfMatch, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotIfMatch = r.Header.Get("If-Match")
				if tc.status == http.StatusNoContent {
					w.Header().Set("ETag", `"new-etag"`)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			cl, err := Dial(srv.URL, "u", "p", nil)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = cl.Close() }()

			e := calendar.Event{ID: eventID("/cal/evt.ics")}
			e.UID = "evt"
			_, err = cl.UpdateEventIfMatch(context.Background(), e, `"good-etag"`)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("UpdateEventIfMatch: %v", err)
			}
			if gotMethod != http.MethodPut {
				t.Errorf("method = %q, want PUT", gotMethod)
			}
			if gotIfMatch != tc.wantHeader {
				t.Errorf("If-Match = %q, want %q", gotIfMatch, tc.wantHeader)
			}
		})
	}
}

// TestUpdateEventIfMatchEmpty rejects an empty If-Match without issuing a
// request — a caller with no precondition must use the unconditional Writer.
func TestUpdateEventIfMatchEmpty(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cl, err := Dial(srv.URL, "u", "p", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()

	if _, err := cl.UpdateEventIfMatch(context.Background(), calendar.Event{ID: eventID("/cal/evt.ics")}, ""); err == nil {
		t.Fatal("want error on empty If-Match, got nil")
	}
	if called {
		t.Error("server was called; want no request on empty If-Match")
	}
}

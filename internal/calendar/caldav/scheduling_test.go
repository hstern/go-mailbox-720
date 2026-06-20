package caldav

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSupportsServerScheduling(t *testing.T) {
	tests := []struct {
		name string
		dav  string // the DAV response header the server advertises
		want bool
	}{
		{"native scheduling", "1, 2, 3, calendar-access, calendar-auto-schedule", true},
		{"dumb server", "1, 2, 3, calendar-access", false},
		{"no dav header", "", false},
		{"case-insensitive + spacing", "1,3,Calendar-Auto-Schedule", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodOptions && tc.dav != "" {
					w.Header().Set("DAV", tc.dav)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cl, err := Dial(srv.URL, "u", "p", &Options{})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = cl.Close() }()

			got, err := cl.SupportsServerScheduling(context.Background())
			if err != nil {
				t.Fatalf("SupportsServerScheduling: %v", err)
			}
			if got != tc.want {
				t.Errorf("SupportsServerScheduling = %v, want %v (DAV: %q)", got, tc.want, tc.dav)
			}
		})
	}
}

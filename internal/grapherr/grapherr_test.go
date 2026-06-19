package grapherr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	cases := []struct {
		status     int
		wantStatus int
		wantCode   string
	}{
		{http.StatusNotImplemented, http.StatusNotImplemented, "notImplemented"},
		{http.StatusUnauthorized, http.StatusUnauthorized, "unauthenticated"},
		{http.StatusForbidden, http.StatusForbidden, "forbidden"},
		{499, http.StatusInternalServerError, "generalException"}, // unmapped -> 500
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		Write(rec, tc.status)

		if rec.Code != tc.wantStatus {
			t.Errorf("Write(%d) status = %d, want %d", tc.status, rec.Code, tc.wantStatus)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Write(%d) Content-Type = %q, want application/json", tc.status, ct)
		}
		var ge struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&ge); err != nil {
			t.Fatalf("Write(%d): decode body: %v", tc.status, err)
		}
		if ge.Error.Code != tc.wantCode {
			t.Errorf("Write(%d) code = %q, want %q", tc.status, ge.Error.Code, tc.wantCode)
		}
		if ge.Error.Message == "" {
			t.Errorf("Write(%d) message is empty", tc.status)
		}
	}
}

package jmap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// sessionJSON is a minimal JMAP Session advertising a primary calendars account.
func sessionJSON(apiURL string) string {
	return `{"capabilities":{"urn:ietf:params:jmap:calendars":{}},` +
		`"accounts":{"acc1":{"name":"u","isPersonal":true,"accountCapabilities":{}}},` +
		`"primaryAccounts":{"urn:ietf:params:jmap:calendars":"acc1"},` +
		`"apiUrl":"` + apiURL + `","downloadUrl":"","uploadUrl":"","eventSourceUrl":"","state":"s"}`
}

// jmapServer spins an httptest server: GET <any> returns the session, POST returns
// the supplied api handler's body.
func jmapServer(t *testing.T, api func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sessionJSON(srv.URL + "/jmap")))
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		api(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDialResolvesCalendarAccount(t *testing.T) {
	srv := jmapServer(t, func(w http.ResponseWriter, _ map[string]any) {})
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if cl.accountID != gojmap.ID("acc1") {
		t.Fatalf("accountID = %q, want acc1", cl.accountID)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDialNoCalendarsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"capabilities":{},"accounts":{},"primaryAccounts":{},"apiUrl":"x","state":"s"}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := Dial(srv.URL, "tok", nil); err == nil {
		t.Fatal("expected error when no primary calendars account")
	}
}

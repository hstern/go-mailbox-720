package jmap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// sessionJSON is a minimal JMAP Session advertising a primary calendars account.
// go-jmap's Client.Do always adds urn:ietf:params:jmap:core to Using and checks
// it against RawCapabilities, so core must appear here alongside the calendars
// capability.
func sessionJSON(apiURL string) string {
	return `{"capabilities":{"urn:ietf:params:jmap:core":{},"urn:ietf:params:jmap:calendars":{}},` +
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

// TestDialBasicAuth verifies that Dial with opts.BasicAuth sends
// Authorization: Basic <base64(u:p)> on the session GET request and does NOT
// send a Bearer header. The httptest handler captures the Authorization header
// from the GET (session fetch) for inspection.
func TestDialBasicAuth(t *testing.T) {
	var capturedAuth string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			capturedAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sessionJSON(srv.URL + "/jmap")))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
	}))
	t.Cleanup(srv.Close)

	_, err := Dial(srv.URL, "ignored-token", &Options{
		BasicAuth: &BasicAuthCredentials{Username: "u", Password: "p"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if !strings.HasPrefix(capturedAuth, "Basic ") {
		t.Errorf("Authorization header = %q, want prefix \"Basic \"", capturedAuth)
	}
	if strings.HasPrefix(capturedAuth, "Bearer ") {
		t.Errorf("Authorization header = %q, must not be Bearer when BasicAuth is set", capturedAuth)
	}
}

// TestDialAPIURLOverride verifies that Dial with opts.APIURLOverride replaces
// the apiUrl value from the server's Session resource. The httptest session
// advertises one URL; we override it and assert cl.c.Session.APIURL equals the
// override after Dial.
func TestDialAPIURLOverride(t *testing.T) {
	const externalAPIURL = "https://external.example/jmap"
	srv := jmapServer(t, func(w http.ResponseWriter, _ map[string]any) {})

	cl, err := Dial(srv.URL, "tok", &Options{
		APIURLOverride: externalAPIURL,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if cl.c.Session.APIURL != externalAPIURL {
		t.Errorf("Session.APIURL = %q, want %q", cl.c.Session.APIURL, externalAPIURL)
	}
}

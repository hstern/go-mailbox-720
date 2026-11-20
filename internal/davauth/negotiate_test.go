package davauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fixed(tok string) TokenSource { return func() (string, error) { return tok, nil } }

func TestNegotiateHTTPClientSetsHeader(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusMultiStatus}, nil
	})

	c := NegotiateHTTPClient(base, fixed("YII-base64-apreq"))
	req, err := http.NewRequest("PROPFIND", "http://dav.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("status = %d, want delegated 207", resp.StatusCode)
	}
	if gotAuth != "Negotiate YII-base64-apreq" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Negotiate YII-base64-apreq")
	}
}

// Each request draws a NEW token from the source: the point of MB720-59, since a
// Kerberos acceptor with a replay cache rejects a re-presented AP-REQ.
func TestNegotiateHTTPClientFreshTokenPerRequest(t *testing.T) {
	var seen []string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	n := 0
	c := NegotiateHTTPClient(base, func() (string, error) { n++; return fmt.Sprintf("tok-%d", n), nil })

	for range 3 {
		req, _ := http.NewRequest("GET", "http://dav.example/", nil)
		if _, err := c.Do(req); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}
	want := []string{"Negotiate tok-1", "Negotiate tok-2", "Negotiate tok-3"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("Authorization headers = %v, want %v", seen, want)
	}
}

// A pre-existing Authorization header is overwritten with the Negotiate token,
// and the caller's request is not mutated.
func TestNegotiateHTTPClientOverwritesAuth(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	c := NegotiateHTTPClient(base, fixed("fresh"))

	req, _ := http.NewRequest("GET", "http://dav.example/", nil)
	req.Header.Set("Authorization", "Basic stale")
	if _, err := c.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Negotiate fresh" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Negotiate fresh")
	}
	if got := req.Header.Get("Authorization"); got != "Basic stale" {
		t.Errorf("caller's request mutated: Authorization = %q", got)
	}
}

// A token-source failure fails the request without reaching the backend.
func TestNegotiateHTTPClientSourceError(t *testing.T) {
	sentinel := errors.New("no ticket")
	called := false
	base := clientFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	c := NegotiateHTTPClient(base, func() (string, error) { return "", sentinel })
	req, _ := http.NewRequest("GET", "http://dav.example/", nil)
	if _, err := c.Do(req); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
	if called {
		t.Error("request must not be sent when the token source fails")
	}
}

func TestNegotiateHTTPClientNilBase(t *testing.T) {
	// A nil base must not panic; it defaults to http.DefaultClient.
	if c := NegotiateHTTPClient(nil, fixed("tok")); c == nil {
		t.Fatal("NegotiateHTTPClient(nil, ...) = nil")
	}
}

// With an *http.Client the token is minted per round trip, so a followed
// redirect (which net/http would otherwise re-send with the same Authorization
// header) carries a different token than the request that triggered it.
func TestNegotiateHTTPClientFreshTokenAcrossRedirect(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)

	n := 0
	c := NegotiateHTTPClient(srv.Client(), func() (string, error) { n++; return fmt.Sprintf("tok-%d", n), nil })
	req, _ := http.NewRequest("GET", srv.URL+"/start", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if fmt.Sprint(auths) != "[Negotiate tok-1 Negotiate tok-2]" {
		t.Errorf("Authorization headers = %v, want two distinct tokens", auths)
	}
}

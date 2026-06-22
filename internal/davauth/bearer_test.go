package davauth

import (
	"net/http"
	"testing"
)

// clientFunc adapts a function to the webdav.HTTPClient interface.
type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestBearerHTTPClientSetsHeader(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusMultiStatus}, nil
	})

	c := BearerHTTPClient(base, "tok-123")
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
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
}

// A pre-existing Authorization header is overwritten with the bearer token.
func TestBearerHTTPClientOverwritesAuth(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	c := BearerHTTPClient(base, "fresh")

	req, _ := http.NewRequest("GET", "http://dav.example/", nil)
	req.Header.Set("Authorization", "Basic stale")
	if _, err := c.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer fresh" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer fresh")
	}
}

func TestBearerHTTPClientNilBase(t *testing.T) {
	// A nil base must not panic; it defaults to http.DefaultClient.
	if c := BearerHTTPClient(nil, "tok"); c == nil {
		t.Fatal("BearerHTTPClient(nil, ...) = nil")
	}
}

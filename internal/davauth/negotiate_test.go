package davauth

import (
	"net/http"
	"testing"
)

func TestNegotiateHTTPClientSetsHeader(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusMultiStatus}, nil
	})

	c := NegotiateHTTPClient(base, "YII-base64-apreq")
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

// A pre-existing Authorization header is overwritten with the Negotiate token.
func TestNegotiateHTTPClientOverwritesAuth(t *testing.T) {
	var gotAuth string
	base := clientFunc(func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK}, nil
	})
	c := NegotiateHTTPClient(base, "fresh")

	req, _ := http.NewRequest("GET", "http://dav.example/", nil)
	req.Header.Set("Authorization", "Basic stale")
	if _, err := c.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Negotiate fresh" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Negotiate fresh")
	}
}

func TestNegotiateHTTPClientNilBase(t *testing.T) {
	// A nil base must not panic; it defaults to http.DefaultClient.
	if c := NegotiateHTTPClient(nil, "tok"); c == nil {
		t.Fatal("NegotiateHTTPClient(nil, ...) = nil")
	}
}

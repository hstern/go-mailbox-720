package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoBody is a handler that copies the (post-middleware) request body to the
// response, so a test can observe what the wrapped handler actually reads.
func echoBody(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if enc := r.Header.Get("Content-Encoding"); enc != "" {
			t.Errorf("Content-Encoding still set: %q", enc)
		}
		_, _ = w.Write(b)
	})
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecompressRequestsGzip(t *testing.T) {
	const want = `{"requests":[{"id":"1","method":"GET","url":"/me/messages"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1.0/$batch", bytes.NewReader(gzipBytes(t, want)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")

	// A handler that asserts the gzip framing was stripped, then echoes the body.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enc := r.Header.Get("Content-Encoding"); enc != "" {
			t.Errorf("Content-Encoding still set: %q", enc)
		}
		if cl := r.Header.Get("Content-Length"); cl != "" {
			t.Errorf("Content-Length still set: %q", cl)
		}
		if r.ContentLength != -1 {
			t.Errorf("ContentLength = %d, want -1", r.ContentLength)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		_, _ = w.Write(b)
	})

	rec := httptest.NewRecorder()
	DecompressRequests(inner).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("decompressed body = %q, want %q", got, want)
	}
}

func TestDecompressRequestsPassThrough(t *testing.T) {
	const want = `{"hello":"world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1.0/me/messages", strings.NewReader(want))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	DecompressRequests(echoBody(t)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q (should be untouched)", got, want)
	}
}

func TestDecompressRequestsCapsInflatedSize(t *testing.T) {
	// A small gzip payload that inflates past the cap (a gzip bomb). Reading the
	// decompressed body must fail rather than allocate it all.
	big := bytes.Repeat([]byte("a"), maxDecompressedBytes+1<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1.0/me/messages", bytes.NewReader(gzipBytes(t, string(big))))
	req.Header.Set("Content-Encoding", "gzip")

	var readErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})

	rec := httptest.NewRecorder()
	DecompressRequests(next).ServeHTTP(rec, req)

	if readErr == nil {
		t.Error("reading an over-large inflated body should fail, got nil error")
	}
}

func TestDecompressRequestsMalformedGzip(t *testing.T) {
	// Declares gzip but the body is not valid gzip framing.
	req := httptest.NewRequest(http.MethodPost, "/v1.0/$batch", strings.NewReader("not gzip"))
	req.Header.Set("Content-Encoding", "gzip")

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	rec := httptest.NewRecorder()
	DecompressRequests(next).ServeHTTP(rec, req)

	if called {
		t.Error("next handler should not be reached for malformed gzip")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "badRequest") {
		t.Errorf("body = %q, want a Graph badRequest error", rec.Body.String())
	}
}

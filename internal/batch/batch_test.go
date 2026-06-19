package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ctxKey is a private context key used to assert that the outer request's
// context propagates into each in-process sub-request.
type ctxKey struct{}

// recordingHandler is a fake inner http.Handler. It records the URL path and
// method of every request it serves and returns a canned JSON response keyed by
// "METHOD path". It also reports the propagated context value so tests can
// assert it survived the batch dispatch.
type recordingHandler struct {
	t        *testing.T
	gotPaths []string
	gotCtx   []string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.gotPaths = append(h.gotPaths, r.Method+" "+r.URL.Path)
	if v, ok := r.Context().Value(ctxKey{}).(string); ok {
		h.gotCtx = append(h.gotCtx, v)
	} else {
		h.gotCtx = append(h.gotCtx, "")
	}

	switch r.Method + " " + r.URL.Path {
	case "GET /v1.0/me/messages":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"value":[{"id":"m1"}]}`)
	case "POST /v1.0/me/messages":
		// Echo the request body back so the test can confirm the JSON body and
		// Content-Type were carried through.
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			h.t.Errorf("POST sub-request Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	case "GET /v1.0/me":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"me"}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"missing"}`)
	}
}

// postBatch posts a batch envelope (a raw JSON string) and returns the decoded
// response envelope plus the recorder.
func postBatch(t *testing.T, inner *recordingHandler, body string) (responseEnvelope, *httptest.ResponseRecorder) {
	t.Helper()
	h := Handler(inner, "/v1.0")
	ctx := context.WithValue(context.Background(), ctxKey{}, "mailbox-id")
	req := httptest.NewRequest(http.MethodPost, "/v1.0/$batch", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)

	var env responseEnvelope
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode response envelope: %v (body=%s)", err, rec.Body.String())
		}
	}
	return env, rec
}

func TestHandlerDispatchesSubRequests(t *testing.T) {
	inner := &recordingHandler{t: t}
	// "3" dependsOn "1": it must run after the GET even though it is listed first
	// would not matter here, but we order it last and also add a write.
	body := `{"requests":[
		{"id":"1","method":"GET","url":"/me/messages"},
		{"id":"2","method":"POST","url":"/me/messages","body":{"subject":"hi"}},
		{"id":"3","method":"GET","url":"/me","dependsOn":["1","2"]}
	]}`

	env, rec := postBatch(t, inner, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	if len(env.Responses) != 3 {
		t.Fatalf("got %d responses, want 3", len(env.Responses))
	}

	byID := map[string]response{}
	for _, r := range env.Responses {
		byID[r.ID] = r
	}

	if got := byID["1"].Status; got != http.StatusOK {
		t.Errorf("response 1 status = %d, want 200", got)
	}
	if got := string(byID["1"].Body); got != `{"value":[{"id":"m1"}]}` {
		t.Errorf("response 1 body = %s", got)
	}
	if got := byID["2"].Status; got != http.StatusCreated {
		t.Errorf("response 2 status = %d, want 201", got)
	}
	if got := string(byID["2"].Body); got != `{"subject":"hi"}` {
		t.Errorf("response 2 body = %s, want echoed JSON body", got)
	}
	if got := byID["3"].Status; got != http.StatusOK {
		t.Errorf("response 3 status = %d, want 200", got)
	}

	// dependsOn ordering: "3" depends on "1" and "2", so it must be served last
	// by inner. Find positions in the recorded dispatch order.
	pos := map[string]int{}
	for i, p := range inner.gotPaths {
		switch p {
		case "GET /v1.0/me/messages":
			pos["1"] = i
		case "POST /v1.0/me/messages":
			pos["2"] = i
		case "GET /v1.0/me":
			pos["3"] = i
		}
	}
	if pos["3"] <= pos["1"] || pos["3"] <= pos["2"] {
		t.Errorf("dependsOn not honored: dispatch order = %v", inner.gotPaths)
	}

	// URL prefixing: every recorded path starts with the basePath.
	for _, p := range inner.gotPaths {
		if !strings.Contains(p, " /v1.0/") {
			t.Errorf("sub-request path %q is not basePath-prefixed", p)
		}
	}

	// Context propagation: the outer mailbox-id reached every sub-request.
	for i, v := range inner.gotCtx {
		if v != "mailbox-id" {
			t.Errorf("sub-request %d context value = %q, want mailbox-id", i, v)
		}
	}
}

func TestHandlerMalformedEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", `{not json`},
		{"unknown dependsOn", `{"requests":[{"id":"1","method":"GET","url":"/me","dependsOn":["nope"]}]}`},
		{"cycle", `{"requests":[
			{"id":"1","method":"GET","url":"/me","dependsOn":["2"]},
			{"id":"2","method":"GET","url":"/me","dependsOn":["1"]}
		]}`},
		{"missing id", `{"requests":[{"method":"GET","url":"/me"}]}`},
		{"duplicate id", `{"requests":[
			{"id":"1","method":"GET","url":"/me"},
			{"id":"1","method":"GET","url":"/me/messages"}
		]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingHandler{t: t}
			_, rec := postBatch(t, inner, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if len(inner.gotPaths) != 0 {
				t.Errorf("inner handler ran %d sub-requests on a malformed envelope; want 0", len(inner.gotPaths))
			}
		})
	}
}

func TestHandlerTooManyRequests(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"requests":[`)
	for i := 0; i <= maxBatchRequests; i++ { // one over the cap
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"%d","method":"GET","url":"/me"}`, i)
	}
	b.WriteString(`]}`)

	inner := &recordingHandler{t: t}
	_, rec := postBatch(t, inner, b.String())
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for more than %d requests", rec.Code, maxBatchRequests)
	}
	if len(inner.gotPaths) != 0 {
		t.Errorf("inner ran %d sub-requests over the cap; want 0", len(inner.gotPaths))
	}
}

func TestHandlerNonJSONBodyOmitted(t *testing.T) {
	inner := &recordingHandler{t: t}
	// "/unknown" is served by the default branch as a 404 with a JSON body; a
	// path with a non-JSON content type would be dropped. Cover the drop path by
	// having inner return text for a specific route.
	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json")
	})
	h := Handler(textHandler, "/v1.0")
	req := httptest.NewRequest(http.MethodPost, "/v1.0/$batch",
		strings.NewReader(`{"requests":[{"id":"1","method":"GET","url":"/x"}]}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	var env responseEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Responses) != 1 {
		t.Fatalf("got %d responses, want 1", len(env.Responses))
	}
	if env.Responses[0].Body != nil {
		t.Errorf("non-JSON sub-response body should be omitted, got %s", env.Responses[0].Body)
	}
	_ = inner
}

func TestIsJSON(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/vnd.api+json", true},
		{"text/plain", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isJSON(tc.ct); got != tc.want {
			t.Errorf("isJSON(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}

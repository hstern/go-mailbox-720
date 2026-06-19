// Package batch implements Microsoft Graph's JSON batching endpoint,
// POST /$batch (https://learn.microsoft.com/graph/json-batching).
//
// A client POSTs a single request whose JSON body bundles several sub-requests
// and receives a single response bundling their results:
//
//	{"requests":[{"id":"1","method":"GET","url":"/me/messages","headers":{...},"body":{...},"dependsOn":["..."]}, ...]}
//	{"responses":[{"id":"1","status":200,"headers":{...},"body":{...}}, ...]}
//
// Each sub-request's url is relative to the service root (e.g. "/me/messages",
// not "/v1.0/me/messages"); Handler prefixes it with the configured basePath
// before dispatching to the inner handler. Sub-requests are executed in-process
// against that handler — not over the network — and they carry the OUTER
// request's context, so cross-cutting middleware state (notably the auth
// middleware's mailbox identity) propagates into every sub-request.
//
// The package is stdlib-only (net/http, net/http/httptest, encoding/json).
package batch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

// request is a single sub-request in a batch envelope. body is arbitrary JSON
// (present only for write methods); dependsOn lists the ids that must complete
// before this sub-request runs.
type request struct {
	ID        string            `json:"id"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      json.RawMessage   `json:"body,omitempty"`
	DependsOn []string          `json:"dependsOn,omitempty"`
}

// requestEnvelope is the decoded POST /$batch body.
type requestEnvelope struct {
	Requests []request `json:"requests"`
}

// response is a single sub-response in the batch result. Body is the decoded
// sub-response JSON when it is application/json, and omitted otherwise.
type response struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// responseEnvelope is the rendered /$batch result body.
type responseEnvelope struct {
	Responses []response `json:"responses"`
}

// Handler returns the POST /$batch handler. It decodes the batch envelope and,
// for each sub-request, builds an in-process *http.Request (method; URL =
// basePath + sub.url; decoded JSON body with the application/json Content-Type;
// copied headers) carrying the outer request's context, serves it against inner
// via an httptest.NewRecorder, and collects {id, status, headers, body} into the
// responses envelope.
//
// Because every sub-request is built from r.Context(), middleware state on the
// outer request (such as the authenticated mailbox identity) is visible to each
// sub-request and thus to inner.
//
// dependsOn is honored: sub-requests run respecting dependency order. This first
// cut executes sequentially in a topologically sorted order rather than in
// parallel — independent sub-requests are not run concurrently — which trivially
// satisfies every dependsOn constraint. A malformed envelope (bad JSON, a
// missing/unknown dependsOn id, or a dependency cycle) is rejected with 400.
// Bounds on a batch. A $batch is one authenticated request that fans out into
// many in-process sub-requests, so without limits a single request is a memory
// and CPU amplification vector (and a long dependsOn chain is unbounded
// recursion). Microsoft Graph caps a batch at 20 sub-requests.
const (
	maxBatchRequests = 20
	maxBatchBytes    = 4 << 20 // 4 MiB
)

func Handler(inner http.Handler, basePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBatchBytes)
		var env requestEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			writeError(w, http.StatusBadRequest, "the batch request body is not valid JSON")
			return
		}
		if len(env.Requests) > maxBatchRequests {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("a batch may contain at most %d requests", maxBatchRequests))
			return
		}

		order, err := executionOrder(env.Requests)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		responses := make([]response, 0, len(env.Requests))
		for _, idx := range order {
			responses = append(responses, dispatch(r, inner, basePath, env.Requests[idx]))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(responseEnvelope{Responses: responses})
	}
}

// dispatch builds and serves a single sub-request against inner, returning its
// assembled response. The sub-request carries outer.Context() so middleware
// state on the batch request propagates into it.
func dispatch(outer *http.Request, inner http.Handler, basePath string, sub request) response {
	method := sub.Method
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader *bytes.Reader
	if len(sub.Body) > 0 {
		bodyReader = bytes.NewReader(sub.Body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, basePath+sub.URL, bodyReader)
	req = req.WithContext(outer.Context())
	for k, v := range sub.Headers {
		req.Header.Set(k, v)
	}
	// A decoded JSON body is, by definition, application/json. Set it unless the
	// caller specified their own Content-Type in the sub-request headers.
	if len(sub.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, req)

	res := response{
		ID:      sub.ID,
		Status:  rec.Code,
		Headers: flattenHeaders(rec.Header()),
	}
	if body := rec.Body.Bytes(); len(body) > 0 && isJSON(rec.Header().Get("Content-Type")) {
		// Compact and validate; a non-JSON body (despite the header) is dropped
		// rather than corrupting the envelope.
		var buf bytes.Buffer
		if json.Compact(&buf, body) == nil {
			res.Body = json.RawMessage(buf.Bytes())
		}
	}
	return res
}

// executionOrder returns indices into reqs in an order that respects every
// dependsOn edge (dependencies before dependents). It errors on a missing id, a
// reference to an unknown id, or a dependency cycle.
func executionOrder(reqs []request) ([]int, error) {
	index := make(map[string]int, len(reqs))
	for i, req := range reqs {
		if req.ID == "" {
			return nil, fmt.Errorf("sub-request at position %d is missing an id", i)
		}
		if _, dup := index[req.ID]; dup {
			return nil, fmt.Errorf("duplicate sub-request id %q", req.ID)
		}
		index[req.ID] = i
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make([]int, len(reqs))
	order := make([]int, 0, len(reqs))

	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("dependsOn cycle involving sub-request id %q", reqs[i].ID)
		}
		state[i] = visiting
		for _, dep := range reqs[i].DependsOn {
			j, ok := index[dep]
			if !ok {
				return fmt.Errorf("sub-request id %q dependsOn unknown id %q", reqs[i].ID, dep)
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		state[i] = done
		order = append(order, i)
		return nil
	}

	for i := range reqs {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// flattenHeaders collapses an http.Header (multi-valued) into the single-valued
// map shape Graph uses in batch sub-responses, joining repeats with ", ".
func flattenHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// isJSON reports whether a Content-Type names a JSON media type.
func isJSON(contentType string) bool {
	mediaType := contentType
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// writeError renders a Graph-shaped error object for a rejected batch envelope.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {
			"code":    "invalidRequest",
			"message": message,
		},
	})
}

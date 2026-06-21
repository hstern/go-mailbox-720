package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// dialTest returns a Client wired to a jmapServer with the given POST handler.
func dialTest(t *testing.T, api func(w http.ResponseWriter, body map[string]any)) *Client {
	t.Helper()
	srv := jmapServer(t, api)
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cl
}

// respond writes a JMAP method-response envelope wrapping one invocation.
func respond(w http.ResponseWriter, method string, args any) {
	a, err := json.Marshal(args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"methodResponses": []any{[]any{method, json.RawMessage(a), "c0"}},
		"sessionState":    "s",
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestListCalendars(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, _ map[string]any) {
		respond(w, "Calendar/get", map[string]any{
			"accountId": "acc1", "state": "1",
			"list": []map[string]any{{"id": "c1", "name": "Personal", "description": "mine"}},
		})
	})
	cals, err := cl.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].ID != "c1" || cals[0].Name != "Personal" || cals[0].Description != "mine" {
		t.Fatalf("cals = %+v", cals)
	}
	_ = gojmap.ID("")
}

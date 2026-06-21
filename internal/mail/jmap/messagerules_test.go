package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

const sieveAccount = "sieve-acct"

// sieveFake is a JMAP server that advertises the Sieve capability and serves the
// SieveScript methods plus blob upload/download over an in-memory store, so the mail
// backend's FilterReader/FilterWriter can be driven end to end — through the real
// translator and the real SieveScript transport. When noSieve is set the session
// omits the Sieve capability, standing in for a server without RFC 9661 support.
type sieveFake struct {
	scripts map[string]map[string]any // id -> {id,name,blobId,isActive}
	blobs   map[string]string
	seq     int
	noSieve bool
}

func newSieveFake() *sieveFake {
	return &sieveFake{scripts: map[string]map[string]any{}, blobs: map[string]string{}}
}

func (f *sieveFake) start(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/session", f.handleSession)
	mux.HandleFunc("/api", f.handleAPI)
	mux.HandleFunc("/upload/", f.handleUpload)
	mux.HandleFunc("/download/", f.handleDownload)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gc := &gojmap.Client{SessionEndpoint: srv.URL + "/session"}
	gc.WithAccessToken("test-token")
	if err := gc.Authenticate(); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return newClient(gc, testAccount) // mail account; the sieve account comes from the session
}

func (f *sieveFake) handleSession(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	caps := map[string]any{"urn:ietf:params:jmap:core": map[string]any{}}
	primary := map[string]any{}
	if !f.noSieve {
		caps["urn:ietf:params:jmap:sieve"] = map[string]any{}
		primary["urn:ietf:params:jmap:sieve"] = sieveAccount
	}
	writeJSON(w, map[string]any{
		"capabilities":    caps,
		"accounts":        map[string]any{testAccount: map[string]any{"name": "Test"}, sieveAccount: map[string]any{"name": "Sieve"}},
		"primaryAccounts": primary,
		"username":        "test",
		"apiUrl":          base + "/api",
		"downloadUrl":     base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":       base + "/upload/{accountId}",
		"eventSourceUrl":  base + "/events",
		"state":           "s",
	})
}

func (f *sieveFake) handleUpload(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.seq++
	blobID := fmt.Sprintf("blob-%d", f.seq)
	f.blobs[blobID] = string(body)
	writeJSON(w, map[string]any{"accountId": sieveAccount, "blobId": blobID, "type": "application/sieve", "size": len(body)})
}

func (f *sieveFake) handleDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	content, ok := f.blobs[parts[1]]
	if !ok {
		http.Error(w, "no blob", http.StatusNotFound)
		return
	}
	_, _ = io.WriteString(w, content)
}

func (f *sieveFake) handleAPI(w http.ResponseWriter, r *http.Request) {
	var req rawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := &gojmap.Response{SessionState: "s"}
	for _, call := range req.Calls {
		args := f.dispatch(call)
		name := call.Name
		if _, ok := args.(*gojmap.MethodError); ok {
			name = "error"
		}
		resp.Responses = append(resp.Responses, &gojmap.Invocation{Name: name, Args: args, CallID: call.CallID})
	}
	writeJSON(w, resp)
}

func (f *sieveFake) dispatch(call rawCall) any {
	switch call.Name {
	case "SieveScript/get":
		list := make([]map[string]any, 0, len(f.scripts))
		for _, s := range f.scripts {
			list = append(list, s)
		}
		return map[string]any{"accountId": sieveAccount, "state": "s", "list": list}
	case "SieveScript/set":
		return f.handleSet(call.Args)
	}
	return &gojmap.MethodError{Type: "unknownMethod"}
}

func (f *sieveFake) handleSet(raw json.RawMessage) any {
	var probe struct {
		Create     map[string]map[string]any `json:"create"`
		Update     map[string]map[string]any `json:"update"`
		Destroy    []string                  `json:"destroy"`
		Activate   *string                   `json:"onSuccessActivateScript"`
		Deactivate bool                      `json:"onSuccessDeactivateScript"`
	}
	_ = json.Unmarshal(raw, &probe)

	resp := map[string]any{"accountId": sieveAccount, "newState": "s"}
	created := map[string]any{}
	for cid, c := range probe.Create {
		f.seq++
		id := fmt.Sprintf("script-%d", f.seq)
		s := map[string]any{"id": id, "name": c["name"], "blobId": c["blobId"], "isActive": false}
		f.scripts[id] = s
		created[cid] = s
		if probe.Activate != nil && *probe.Activate == "#"+cid {
			f.activate(id)
		}
	}
	if len(created) > 0 {
		resp["created"] = created
	}
	updated := map[string]any{}
	for id, patch := range probe.Update {
		if s, ok := f.scripts[id]; ok {
			if blob, ok := patch["blobId"].(string); ok {
				s["blobId"] = blob
			}
			updated[id] = nil
		}
	}
	if len(updated) > 0 {
		resp["updated"] = updated
	}
	if probe.Deactivate {
		for _, s := range f.scripts {
			s["isActive"] = false
		}
	}
	if probe.Activate != nil && !strings.HasPrefix(*probe.Activate, "#") {
		f.activate(*probe.Activate)
	}
	for _, id := range probe.Destroy {
		delete(f.scripts, id)
	}
	if len(probe.Destroy) > 0 {
		resp["destroyed"] = probe.Destroy
	}
	return resp
}

func (f *sieveFake) activate(id string) {
	for sid, s := range f.scripts {
		s["isActive"] = sid == id
	}
}

func sampleRule(name string) port.MessageRule {
	return port.MessageRule{
		DisplayName: name, Enabled: true,
		Conditions: port.RuleConditions{SubjectContains: []string{"urgent"}},
		Actions:    port.RuleActions{MoveToFolder: "Priority", MarkAsRead: true},
	}
}

func TestJMAPFilterCreateListGet(t *testing.T) {
	cl := newSieveFake().start(t)
	ctx := context.Background()

	// Empty mailbox: no active script, no rules.
	if rules, err := cl.ListRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("initial ListRules = %v, %v; want empty", rules, err)
	}

	created, err := cl.CreateRule(ctx, sampleRule("From boss"))
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created rule has no id")
	}

	// It reads back through a real Sieve round-trip (encode -> upload -> download -> decode).
	rules, err := cl.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(rules) != 1 || rules[0].DisplayName != "From boss" {
		t.Fatalf("rules = %+v", rules)
	}
	if len(rules[0].Conditions.SubjectContains) != 1 || rules[0].Conditions.SubjectContains[0] != "urgent" {
		t.Errorf("conditions lost in round-trip: %+v", rules[0].Conditions)
	}
	if !rules[0].Actions.MarkAsRead || rules[0].Actions.MoveToFolder != "Priority" {
		t.Errorf("actions lost in round-trip: %+v", rules[0].Actions)
	}

	got, err := cl.GetRule(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetRule = %+v, %v", got, err)
	}
	if _, err := cl.GetRule(ctx, "nope"); !errors.Is(err, port.ErrRuleNotFound) {
		t.Errorf("GetRule(missing) err = %v, want ErrRuleNotFound", err)
	}
}

func TestJMAPFilterUpdate(t *testing.T) {
	cl := newSieveFake().start(t)
	ctx := context.Background()
	created, _ := cl.CreateRule(ctx, sampleRule("orig"))

	upd := created
	upd.DisplayName = "renamed"
	upd.Enabled = false
	if _, err := cl.UpdateRule(ctx, created.ID, upd); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	got, err := cl.GetRule(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRule: %v", err)
	}
	if got.DisplayName != "renamed" || got.Enabled {
		t.Errorf("update not applied: %+v", got)
	}

	if _, err := cl.UpdateRule(ctx, "ghost", sampleRule("x")); !errors.Is(err, port.ErrRuleNotFound) {
		t.Errorf("UpdateRule(missing) err = %v, want ErrRuleNotFound", err)
	}
}

func TestJMAPFilterDelete(t *testing.T) {
	cl := newSieveFake().start(t)
	ctx := context.Background()
	a, _ := cl.CreateRule(ctx, sampleRule("a"))
	_, _ = cl.CreateRule(ctx, sampleRule("b"))

	if err := cl.DeleteRule(ctx, a.ID); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	rules, _ := cl.ListRules(ctx)
	if len(rules) != 1 || rules[0].DisplayName != "b" {
		t.Fatalf("after delete: %+v", rules)
	}
	if err := cl.DeleteRule(ctx, a.ID); !errors.Is(err, port.ErrRuleNotFound) {
		t.Errorf("DeleteRule(missing) err = %v, want ErrRuleNotFound", err)
	}
}

func TestJMAPFilterIDsAreUnique(t *testing.T) {
	cl := newSieveFake().start(t)
	ctx := context.Background()
	a, _ := cl.CreateRule(ctx, sampleRule("a"))
	b, _ := cl.CreateRule(ctx, sampleRule("b"))
	if a.ID == b.ID {
		t.Errorf("two rules share id %q", a.ID)
	}
}

func TestJMAPFiltersUnsupported(t *testing.T) {
	f := newSieveFake()
	f.noSieve = true
	cl := f.start(t)
	if _, err := cl.ListRules(context.Background()); !errors.Is(err, port.ErrFiltersUnsupported) {
		t.Errorf("ListRules without sieve = %v, want ErrFiltersUnsupported", err)
	}
	if _, err := cl.CreateRule(context.Background(), sampleRule("x")); !errors.Is(err, port.ErrFiltersUnsupported) {
		t.Errorf("CreateRule without sieve = %v, want ErrFiltersUnsupported", err)
	}
}

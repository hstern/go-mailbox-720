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
)

const testAccount = "acct-1"

var errBadCall = errors.New("jmap test: malformed method call")

// fakeServer is a minimal in-memory JMAP Sieve server: it serves a session, the
// blob upload/download endpoints, and the SieveScript/get|set|validate methods, over
// the real go-jmap client + wire encoding, so the tests exercise request building,
// blob round-tripping, and response parsing end to end. A script whose body contains
// "INVALID" is rejected by the server, standing in for a real Sieve grammar error.
type fakeServer struct {
	scripts map[gojmap.ID]*sieveScript
	blobs   map[gojmap.ID]string
	seq     int
}

func newFixture() *fakeServer {
	return &fakeServer{
		scripts: map[gojmap.ID]*sieveScript{},
		blobs:   map[gojmap.ID]string{},
	}
}

func (f *fakeServer) start(t *testing.T) *Client {
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
	return newClient(gc, testAccount)
}

func (f *fakeServer) handleSession(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	writeJSON(w, map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:core": map[string]any{},
			string(sieveURI):            map[string]any{},
		},
		"accounts":        map[string]any{testAccount: map[string]any{"name": "Test", "isPersonal": true}},
		"primaryAccounts": map[string]any{string(sieveURI): testAccount},
		"username":        "test",
		"apiUrl":          base + "/api",
		"downloadUrl":     base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":       base + "/upload/{accountId}",
		"eventSourceUrl":  base + "/events",
		"state":           "session-state",
	})
}

func (f *fakeServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if ct := r.Header.Get("Content-Type"); ct != "application/sieve" {
		http.Error(w, "want application/sieve, got "+ct, http.StatusUnsupportedMediaType)
		return
	}
	f.seq++
	blobID := gojmap.ID(fmt.Sprintf("blob-%d", f.seq))
	f.blobs[blobID] = string(body)
	writeJSON(w, map[string]any{
		"accountId": testAccount, "blobId": blobID,
		"type": "application/sieve", "size": len(body),
	})
}

func (f *fakeServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	// path: /download/{accountId}/{blobId}/{name}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad download path", http.StatusBadRequest)
		return
	}
	content, ok := f.blobs[gojmap.ID(parts[1])]
	if !ok {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/sieve")
	_, _ = io.WriteString(w, content)
}

func (f *fakeServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	var req rawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := &gojmap.Response{SessionState: "session-state"}
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

func (f *fakeServer) dispatch(call rawCall) any {
	switch call.Name {
	case "SieveScript/get":
		list := make([]*sieveScript, 0, len(f.scripts))
		for _, s := range f.scripts {
			list = append(list, s)
		}
		return map[string]any{"accountId": testAccount, "state": "s0", "list": list}
	case "SieveScript/set":
		return f.handleSet(call.Args)
	case "SieveScript/validate":
		var m sieveValidate
		_ = json.Unmarshal(call.Args, &m)
		if strings.Contains(f.blobs[m.BlobID], "INVALID") {
			desc := "line 1: unknown command 'INVALID'"
			return map[string]any{"accountId": testAccount, "error": &gojmap.SetError{Type: "invalidSieve", Description: &desc}}
		}
		return map[string]any{"accountId": testAccount, "error": nil}
	}
	return &gojmap.MethodError{Type: "unknownMethod"}
}

func (f *fakeServer) handleSet(raw json.RawMessage) any {
	var m sieveSet
	_ = json.Unmarshal(raw, &m)
	resp := map[string]any{"accountId": testAccount, "newState": "s1"}

	created := map[string]*sieveScript{}
	notCreated := map[string]*gojmap.SetError{}
	for cid, c := range m.Create {
		// Reject a script whose blob body is invalid, like a real server would.
		if strings.Contains(f.blobs[c.BlobID], "INVALID") {
			desc := "script content violates Sieve grammar"
			notCreated[cid] = &gojmap.SetError{Type: "invalidSieve", Description: &desc}
			continue
		}
		f.seq++
		id := gojmap.ID(fmt.Sprintf("script-%d", f.seq))
		s := &sieveScript{ID: id, Name: c.Name, BlobID: c.BlobID}
		f.scripts[id] = s
		created[cid] = s
		// onSuccessActivateScript may reference this creation id as "#cid".
		if m.ActivateScript != nil && *m.ActivateScript == gojmap.ID("#"+cid) {
			f.activate(id)
		}
	}
	if len(created) > 0 {
		resp["created"] = created
	}
	if len(notCreated) > 0 {
		resp["notCreated"] = notCreated
	}

	updated := map[gojmap.ID]*sieveScript{}
	for id, patch := range m.Update {
		s, ok := f.scripts[id]
		if !ok {
			continue
		}
		if blob, ok := patch["blobId"].(string); ok {
			s.BlobID = gojmap.ID(blob)
		}
		updated[id] = nil // null: no extra server-set changes
	}

	if m.DeactivateScript {
		for _, s := range f.scripts {
			s.IsActive = false
		}
	}
	if m.ActivateScript != nil && !strings.HasPrefix(string(*m.ActivateScript), "#") {
		f.activate(*m.ActivateScript)
	}

	for _, id := range m.Destroy {
		delete(f.scripts, id)
	}
	if len(m.Destroy) > 0 {
		resp["destroyed"] = m.Destroy
	}
	if len(updated) > 0 {
		resp["updated"] = updated
	}
	return resp
}

// activate makes id the sole active script.
func (f *fakeServer) activate(id gojmap.ID) {
	for sid, s := range f.scripts {
		s.IsActive = sid == id
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type rawRequest struct {
	Calls []rawCall `json:"methodCalls"`
}

type rawCall struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

func (c *rawCall) UnmarshalJSON(data []byte) error {
	var triple []json.RawMessage
	if err := json.Unmarshal(data, &triple); err != nil {
		return err
	}
	if len(triple) != 3 {
		return errBadCall
	}
	if err := json.Unmarshal(triple[0], &c.Name); err != nil {
		return err
	}
	c.Args = triple[1]
	return json.Unmarshal(triple[2], &c.CallID)
}

const sampleScript = `require ["fileinto"];
if header :contains "subject" "urgent" {
  fileinto "INBOX/Priority";
}
`

func TestPutActivateAndRead(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()

	created, err := cl.PutScript(ctx, "rules", sampleScript)
	if err != nil {
		t.Fatalf("PutScript: %v", err)
	}
	if created.ID == "" || created.Name != "rules" || created.BlobID == "" {
		t.Fatalf("created = %+v", created)
	}
	if created.IsActive {
		t.Error("a freshly created script must be inactive until activated")
	}

	if err := cl.Activate(ctx, created.ID); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// The active script reads back with the uploaded content, via blob round-trip.
	got, content, ok, err := cl.ActiveScript(ctx)
	if err != nil {
		t.Fatalf("ActiveScript: %v", err)
	}
	if !ok {
		t.Fatal("expected an active script after Activate")
	}
	if got.ID != created.ID {
		t.Errorf("active id = %q, want %q", got.ID, created.ID)
	}
	if content != sampleScript {
		t.Errorf("content round-trip mismatch:\n got %q\nwant %q", content, sampleScript)
	}
}

func TestListScripts(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()
	if _, err := cl.PutScript(ctx, "a", sampleScript); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := cl.PutScript(ctx, "b", sampleScript); err != nil {
		t.Fatalf("put b: %v", err)
	}
	scripts, err := cl.ListScripts(ctx)
	if err != nil {
		t.Fatalf("ListScripts: %v", err)
	}
	if len(scripts) != 2 {
		t.Fatalf("script count = %d, want 2", len(scripts))
	}
}

func TestUpdateScriptContent(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()
	created, err := cl.PutScript(ctx, "rules", sampleScript)
	if err != nil {
		t.Fatalf("PutScript: %v", err)
	}
	const updated = `require ["imap4flags"]; setflag "\\Seen";` + "\n"
	if err := cl.UpdateScriptContent(ctx, created.ID, updated); err != nil {
		t.Fatalf("UpdateScriptContent: %v", err)
	}
	// Read the script's new content back through its (new) blob.
	scripts, err := cl.ListScripts(ctx)
	if err != nil {
		t.Fatalf("ListScripts: %v", err)
	}
	content, err := cl.ScriptContent(ctx, scripts[0].BlobID)
	if err != nil {
		t.Fatalf("ScriptContent: %v", err)
	}
	if content != updated {
		t.Errorf("updated content = %q, want %q", content, updated)
	}
}

func TestDeactivate(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()
	created, _ := cl.PutScript(ctx, "rules", sampleScript)
	if err := cl.Activate(ctx, created.ID); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := cl.Deactivate(ctx); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, _, ok, err := cl.ActiveScript(ctx); err != nil || ok {
		t.Errorf("after Deactivate: ok=%v err=%v, want no active script", ok, err)
	}
}

func TestDeleteScript(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()
	created, _ := cl.PutScript(ctx, "rules", sampleScript)
	if err := cl.DeleteScript(ctx, created.ID); err != nil {
		t.Fatalf("DeleteScript: %v", err)
	}
	scripts, err := cl.ListScripts(ctx)
	if err != nil {
		t.Fatalf("ListScripts: %v", err)
	}
	if len(scripts) != 0 {
		t.Errorf("script not deleted, %d remain", len(scripts))
	}
}

func TestPutScriptRejectsInvalid(t *testing.T) {
	cl := newFixture().start(t)
	// The server reports notCreated for a script that fails its Sieve check; PutScript
	// must surface that as an error rather than a phantom success.
	if _, err := cl.PutScript(context.Background(), "bad", "INVALID nonsense;\n"); err == nil {
		t.Error("PutScript with invalid content = nil error, want a notCreated error")
	}
}

func TestValidateRejectsBadScript(t *testing.T) {
	f := newFixture()
	cl := f.start(t)
	ctx := context.Background()
	if err := cl.Validate(ctx, sampleScript); err != nil {
		t.Errorf("Validate(valid) = %v, want nil", err)
	}
	if err := cl.Validate(ctx, "INVALID nonsense;\n"); err == nil {
		t.Error("Validate(invalid) = nil, want an error")
	}
}

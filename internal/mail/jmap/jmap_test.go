package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

const testAccount = "acct-1"

var errBadCall = errors.New("jmap test: malformed method call")

// fakeServer is a minimal in-memory JMAP server for the adapter tests. It serves
// a session document and the handful of Mail methods the adapter calls over the
// real go-jmap client + wire encoding, so the tests exercise request building,
// response parsing, and the object mapping end to end.
type fakeServer struct {
	mailboxes []*mailbox.Mailbox
	emails    map[gojmap.ID]*email.Email
	blobs     map[gojmap.ID][]byte // blobId -> raw bytes
	state     string               // current Email state

	// changes records account Email changes keyed by the state they advance FROM.
	created   map[string][]gojmap.ID
	updated   map[string][]gojmap.ID
	destroyed map[string][]gojmap.ID
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		emails:    map[gojmap.ID]*email.Email{},
		blobs:     map[gojmap.ID][]byte{},
		state:     "s0",
		created:   map[string][]gojmap.ID{},
		updated:   map[string][]gojmap.ID{},
		destroyed: map[string][]gojmap.ID{},
	}
}

// start wires the fake server into an httptest.Server and returns a Client
// pointed at it.
func (f *fakeServer) start(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/session", f.handleSession)
	mux.HandleFunc("/api", f.handleAPI)
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
	session := map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:core": map[string]any{},
			string(mail.URI):            map[string]any{},
		},
		"accounts": map[string]any{
			testAccount: map[string]any{"name": "Test", "isPersonal": true},
		},
		"primaryAccounts": map[string]any{
			string(mail.URI): testAccount,
		},
		"username":       "test",
		"apiUrl":         base + "/api",
		"downloadUrl":    base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":      base + "/upload",
		"eventSourceUrl": base + "/events",
		"state":          "session-state",
	}
	writeJSON(w, session)
}

func (f *fakeServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Path: /download/{accountId}/{blobId}/{name}
	parts := splitPath(r.URL.Path)
	if len(parts) < 3 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	blob, ok := f.blobs[gojmap.ID(parts[2])]
	if !ok {
		http.Error(w, "no blob", http.StatusNotFound)
		return
	}
	_, _ = w.Write(blob)
}

// rawRequest mirrors the JMAP request wire shape, but keeps each method call's
// arguments as raw JSON so the fake server can decode them into the request
// structs (go-jmap's Invocation decoder builds RESPONSE types from method names,
// which is the wrong direction for a server).
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

func (f *fakeServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	var req rawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := &gojmap.Response{SessionState: "session-state"}
	for _, call := range req.Calls {
		args := f.dispatch(call)
		resp.Responses = append(resp.Responses, &gojmap.Invocation{
			Name:   responseName(call.Name, args),
			Args:   args,
			CallID: call.CallID,
		})
	}
	writeJSON(w, resp)
}

// dispatch handles a single method call, returning the response args object.
func (f *fakeServer) dispatch(call rawCall) any {
	switch call.Name {
	case "Mailbox/get":
		return &mailbox.GetResponse{Account: testAccount, State: "mbx-state", List: f.mailboxes}
	case "Email/query":
		var m queryArgs
		_ = json.Unmarshal(call.Args, &m)
		return f.query(&m)
	case "Email/get":
		var m email.Get
		_ = json.Unmarshal(call.Args, &m)
		return f.get(&m)
	case "Email/set":
		var m setArgs
		_ = json.Unmarshal(call.Args, &m)
		return f.set(&m)
	case "Email/changes":
		var m email.Changes
		_ = json.Unmarshal(call.Args, &m)
		return f.changes(&m)
	}
	return &gojmap.MethodError{Type: "unknownMethod"}
}

// setArgs decodes an Email/set request. The Update patch values arrive as raw
// JSON (a $seen keyword set to true or null), which the test inspects directly.
type setArgs struct {
	IfInState string                                   `json:"ifInState"`
	Update    map[gojmap.ID]map[string]json.RawMessage `json:"update"`
	Destroy   []gojmap.ID                              `json:"destroy"`
}

// queryArgs decodes an Email/query request, keeping the filter as a recursive
// raw condition the test evaluator understands.
type queryArgs struct {
	Filter   *rawFilter `json:"filter"`
	Position int64      `json:"position"`
	Limit    uint64     `json:"limit"`
}

// rawFilter captures both a FilterCondition and a FilterOperator: Operator is
// set on the operator form, Conditions holds its children; otherwise the
// condition fields apply.
type rawFilter struct {
	Operator   string       `json:"operator"`
	Conditions []*rawFilter `json:"conditions"`
	InMailbox  gojmap.ID    `json:"inMailbox"`
	Subject    string       `json:"subject"`
	HasKeyword string       `json:"hasKeyword"`
	NotKeyword string       `json:"notKeyword"`
}

func (f *fakeServer) query(m *queryArgs) *email.QueryResponse {
	var ids []gojmap.ID
	for _, e := range f.orderedEmails() {
		if matchesFilter(m.Filter, e) {
			ids = append(ids, e.ID)
		}
	}
	if m.Position > 0 && int(m.Position) < len(ids) {
		ids = ids[m.Position:]
	} else if m.Position > 0 {
		ids = nil
	}
	if m.Limit > 0 && uint64(len(ids)) > m.Limit {
		ids = ids[:m.Limit]
	}
	return &email.QueryResponse{Account: testAccount, QueryState: f.state, IDs: ids}
}

func (f *fakeServer) get(m *email.Get) *email.GetResponse {
	resp := &email.GetResponse{Account: testAccount, State: f.state}
	for _, id := range m.IDs {
		if e, ok := f.emails[id]; ok {
			resp.List = append(resp.List, e)
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	return resp
}

func (f *fakeServer) set(m *setArgs) any {
	// ifInState is the account-level optimistic-concurrency guard (RFC 8620 §5.3):
	// reject the whole call with a stateMismatch when it does not match.
	if m.IfInState != "" && m.IfInState != f.state {
		return &gojmap.MethodError{Type: "stateMismatch"}
	}
	resp := &email.SetResponse{Account: testAccount, OldState: f.state}
	for id, patch := range m.Update {
		e, ok := f.emails[id]
		if !ok {
			continue
		}
		if raw, has := patch["keywords/"+keywordSeen]; has {
			if e.Keywords == nil {
				e.Keywords = map[string]bool{}
			}
			if string(raw) == "null" {
				delete(e.Keywords, keywordSeen)
			} else {
				e.Keywords[keywordSeen] = true
			}
		}
		if resp.Updated == nil {
			resp.Updated = map[gojmap.ID]*email.Email{}
		}
		resp.Updated[id] = nil
	}
	for _, id := range m.Destroy {
		delete(f.emails, id)
		resp.Destroyed = append(resp.Destroyed, id)
	}
	f.advanceState()
	resp.NewState = f.state
	return resp
}

func (f *fakeServer) changes(m *email.Changes) any {
	if _, known := f.created[m.SinceState]; !known {
		if _, ku := f.updated[m.SinceState]; !ku {
			if _, kd := f.destroyed[m.SinceState]; !kd {
				if m.SinceState != f.state {
					return &gojmap.MethodError{Type: "cannotCalculateChanges"}
				}
			}
		}
	}
	return &email.ChangesResponse{
		Account:   testAccount,
		OldState:  m.SinceState,
		NewState:  f.state,
		Created:   f.created[m.SinceState],
		Updated:   f.updated[m.SinceState],
		Destroyed: f.destroyed[m.SinceState],
	}
}

func (f *fakeServer) advanceState() {
	f.state += "x"
}

// orderedEmails returns emails newest-first by receivedAt (the sort the adapter
// always requests).
func (f *fakeServer) orderedEmails() []*email.Email {
	out := make([]*email.Email, 0, len(f.emails))
	for _, e := range f.emails {
		out = append(out, e)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if recvAt(out[j]).After(recvAt(out[i])) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func recvAt(e *email.Email) time.Time {
	if e.ReceivedAt == nil {
		return time.Time{}
	}
	return *e.ReceivedAt
}

// matchesFilter is a tiny filter evaluator covering what the tests assert:
// InMailbox membership, a Subject substring, $seen keyword presence/absence, and
// AND/OR/NOT operators.
func matchesFilter(f *rawFilter, e *email.Email) bool {
	if f == nil {
		return true
	}
	if f.Operator != "" {
		switch f.Operator {
		case "AND":
			for _, c := range f.Conditions {
				if !matchesFilter(c, e) {
					return false
				}
			}
			return true
		case "OR":
			for _, c := range f.Conditions {
				if matchesFilter(c, e) {
					return true
				}
			}
			return false
		case "NOT":
			for _, c := range f.Conditions {
				if matchesFilter(c, e) {
					return false
				}
			}
			return true
		}
	}
	if f.InMailbox != "" && !e.MailboxIDs[f.InMailbox] {
		return false
	}
	if f.Subject != "" && !contains(e.Subject, f.Subject) {
		return false
	}
	if f.HasKeyword != "" && !e.Keywords[f.HasKeyword] {
		return false
	}
	if f.NotKeyword != "" && e.Keywords[f.NotKeyword] {
		return false
	}
	return true
}

func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// responseName returns the JMAP response method name for a request, accounting
// for a method-error response carrying the name "error".
func responseName(reqName string, args any) string {
	if _, ok := args.(*gojmap.MethodError); ok {
		return "error"
	}
	return reqName
}

// seedInbox adds an inbox mailbox and one message, returning the inbox id.
func (f *fakeServer) seedInbox() gojmap.ID {
	inbox := &mailbox.Mailbox{ID: "mbx-inbox", Name: "Inbox", Role: mailbox.RoleInbox, TotalEmails: 1, UnreadEmails: 1}
	f.mailboxes = append(f.mailboxes, inbox)
	recv := time.Date(2025, 6, 11, 12, 0, 0, 0, time.UTC)
	sent := time.Date(2025, 6, 11, 11, 59, 0, 0, time.UTC)
	e := &email.Email{
		ID:            "email-1",
		BlobID:        "blob-1",
		MailboxIDs:    map[gojmap.ID]bool{inbox.ID: true},
		Subject:       "Hello there",
		From:          []*mail.Address{{Name: "Alice", Email: "alice@example.com"}},
		To:            []*mail.Address{{Name: "Bob", Email: "bob@example.com"}},
		ReceivedAt:    &recv,
		SentAt:        &sent,
		Preview:       "This is the body",
		HasAttachment: false,
		Keywords:      map[string]bool{},
		TextBody:      []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
		BodyValues:    map[string]*email.BodyValue{"1": {Value: "This is the body of the message."}},
	}
	f.emails[e.ID] = e
	f.blobs[e.BlobID] = []byte("From: Alice\r\n\r\nThis is the body of the message.\r\n")
	return inbox.ID
}

// --- tests ---

func TestDialResolvesPrimaryAccount(t *testing.T) {
	f := newFakeServer()
	cl := f.start(t)
	if cl.accountID != testAccount {
		t.Errorf("accountID = %q, want %q", cl.accountID, testAccount)
	}
}

func TestListMailFolders(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	folders, err := cl.ListMailFolders(context.Background())
	if err != nil {
		t.Fatalf("ListMailFolders: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(folders))
	}
	if folders[0].DisplayName != "Inbox" || folders[0].Total != 1 || folders[0].Unread != 1 {
		t.Errorf("folder = %+v, want Inbox total=1 unread=1", folders[0])
	}
	if folders[0].WellKnownName != "inbox" {
		t.Errorf("folder WellKnownName = %q, want %q (from role)", folders[0].WellKnownName, "inbox")
	}
}

func TestListMessagesInbox(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	msgs, err := cl.ListMessages(context.Background(), "", port.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Subject != "Hello there" {
		t.Errorf("Subject = %q, want Hello there", m.Subject)
	}
	if m.From.Email != "alice@example.com" {
		t.Errorf("From = %q, want alice@example.com", m.From.Email)
	}
	if m.IsRead {
		t.Error("IsRead = true, want false (no $seen keyword)")
	}
	if m.Body.Content != "" {
		t.Error("list should not populate body")
	}
}

func TestListMessagesWithFilter(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	got, err := cl.ListMessages(context.Background(), "", port.Page{}, parseFilter(t, "subject eq 'Hello'"))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("matching filter: got %d, want 1", len(got))
	}
	none, err := cl.ListMessages(context.Background(), "", port.Page{}, parseFilter(t, "subject eq 'Nope'"))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("non-matching filter: got %d, want 0", len(none))
	}
}

func TestGetMessagePopulatesBody(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	id := messageID("email-1")
	m, err := cl.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.Body.Content != "This is the body of the message." {
		t.Errorf("Body = %q, want the message body", m.Body.Content)
	}
	if m.Body.ContentType != "text" {
		t.Errorf("ContentType = %q, want text", m.Body.ContentType)
	}
}

func TestSetReadAddsAndRemovesKeyword(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)
	id := messageID("email-1")

	if err := cl.SetRead(context.Background(), id, true); err != nil {
		t.Fatalf("SetRead(true): %v", err)
	}
	if !f.emails["email-1"].Keywords[keywordSeen] {
		t.Error("after SetRead(true), $seen not set")
	}
	if err := cl.SetRead(context.Background(), id, false); err != nil {
		t.Fatalf("SetRead(false): %v", err)
	}
	if f.emails["email-1"].Keywords[keywordSeen] {
		t.Error("after SetRead(false), $seen still set")
	}
}

func TestSetReadIfMatchHonoursState(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)
	id := messageID("email-1")

	// A message read surfaces the account Email state ("s0") as its coarse ETag.
	msg, err := cl.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.ETag != "s0" {
		t.Fatalf("message ETag = %q, want %q (account Email state)", msg.ETag, "s0")
	}

	// Matching ifMatch applies the read-state change.
	if err := cl.SetReadIfMatch(context.Background(), id, true, "s0"); err != nil {
		t.Fatalf("SetReadIfMatch(matching): %v", err)
	}
	if !f.emails["email-1"].Keywords[keywordSeen] {
		t.Error("after matching SetReadIfMatch, $seen not set")
	}

	// The set advanced the state; the stale ETag now fails the precondition.
	err = cl.SetReadIfMatch(context.Background(), id, false, "s0")
	if !errors.Is(err, port.ErrPreconditionFailed) {
		t.Fatalf("SetReadIfMatch(stale) err = %v, want ErrPreconditionFailed", err)
	}
	// The refused write left the keyword untouched.
	if !f.emails["email-1"].Keywords[keywordSeen] {
		t.Error("refused SetReadIfMatch cleared $seen; want unchanged")
	}

	// An empty ifMatch is rejected before any request.
	if err := cl.SetReadIfMatch(context.Background(), id, false, ""); err == nil {
		t.Error("SetReadIfMatch(empty) err = nil, want error")
	}
}

func TestDeleteMessage(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	if err := cl.DeleteMessage(context.Background(), messageID("email-1")); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if _, ok := f.emails["email-1"]; ok {
		t.Error("message still present after delete")
	}
}

func TestRawMessage(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	raw, err := cl.RawMessage(context.Background(), messageID("email-1"))
	if err != nil {
		t.Fatalf("RawMessage: %v", err)
	}
	if len(raw) == 0 || string(raw[:5]) != "From:" {
		t.Errorf("raw = %q, want the RFC822 blob", raw)
	}
}

func TestDeltaInitialThenIncremental(t *testing.T) {
	f := newFakeServer()
	inbox := f.seedInbox()
	cl := f.start(t)

	// Initial sync: empty token returns the current message + a fresh token.
	changed, removed, token, err := cl.Delta(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Delta(initial): %v", err)
	}
	if len(changed) != 1 || len(removed) != 0 || token == "" {
		t.Fatalf("initial: changed=%d removed=%d token=%q, want 1/0/non-empty", len(changed), len(removed), token)
	}

	// Record a change: a new email created at the current state, then advance.
	prevState := f.state
	recv := time.Date(2025, 6, 12, 9, 0, 0, 0, time.UTC)
	f.emails["email-2"] = &email.Email{
		ID:         "email-2",
		MailboxIDs: map[gojmap.ID]bool{inbox: true},
		Subject:    "Second",
		ReceivedAt: &recv,
		Keywords:   map[string]bool{},
	}
	f.created[prevState] = []gojmap.ID{"email-2"}
	f.advanceState()

	// The initial token was at prevState, so incremental sees email-2 created.
	state, err := decodeDeltaToken(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if state != prevState {
		t.Fatalf("token state = %q, want %q", state, prevState)
	}
	changed, removed, token2, err := cl.Delta(context.Background(), "", token)
	if err != nil {
		t.Fatalf("Delta(incremental): %v", err)
	}
	if len(changed) != 1 || changed[0].Subject != "Second" {
		t.Fatalf("incremental changed = %+v, want [Second]", changed)
	}
	if len(removed) != 0 {
		t.Errorf("incremental removed = %d, want 0", len(removed))
	}
	if token2 == token {
		t.Error("token did not advance after incremental delta")
	}
}

func TestDeltaResyncOnStaleToken(t *testing.T) {
	f := newFakeServer()
	f.seedInbox()
	cl := f.start(t)

	// A token whose state the server can't diff from triggers a full resync,
	// returning the current messages rather than an error.
	stale := encodeDeltaToken("ancient-state")
	changed, removed, token, err := cl.Delta(context.Background(), "", stale)
	if err != nil {
		t.Fatalf("Delta(stale): %v", err)
	}
	if len(changed) != 1 || len(removed) != 0 || token == "" {
		t.Fatalf("resync: changed=%d removed=%d token=%q, want 1/0/non-empty", len(changed), len(removed), token)
	}
}

func TestInterfaceSatisfaction(t *testing.T) {
	// Compile-time assertions live in the source files; this guards them at the
	// test level too, since a consumer type-asserts for these capabilities.
	var b port.Backend = (*Client)(nil)
	if _, ok := b.(port.Writer); !ok {
		t.Error("Client does not satisfy mail.Writer")
	}
	if _, ok := b.(port.DeltaReader); !ok {
		t.Error("Client does not satisfy mail.DeltaReader")
	}
	if _, ok := b.(port.RawReader); !ok {
		t.Error("Client does not satisfy mail.RawReader")
	}
	if _, ok := b.(port.ConditionalWriter); !ok {
		t.Error("Client does not satisfy mail.ConditionalWriter")
	}
}

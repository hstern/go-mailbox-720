package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

const testAccount = "acct-1"

var errBadCall = errors.New("jmap test: malformed method call")

// fakeServer is a minimal in-memory JMAP contacts server: it serves a session and
// the AddressBook/ContactCard read methods the adapter calls, over the real go-jmap
// client + wire encoding, so the tests exercise request building, response parsing,
// and the JSContact → neutral mapping end to end.
type fakeServer struct {
	addressBooks []*addressBook
	cardIDs      []gojmap.ID
	cards        map[gojmap.ID]json.RawMessage // JMAP ContactCard wire JSON, keyed by id

	// ContactCard/changes canned response (delta tests populate these).
	changesNewState  string
	changesCreated   []gojmap.ID
	changesUpdated   []gojmap.ID
	changesDestroyed []gojmap.ID
}

func (f *fakeServer) start(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/session", f.handleSession)
	mux.HandleFunc("/api", f.handleAPI)
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
			string(contactsURI):         map[string]any{},
		},
		"accounts":        map[string]any{testAccount: map[string]any{"name": "Test", "isPersonal": true}},
		"primaryAccounts": map[string]any{string(contactsURI): testAccount},
		"username":        "test",
		"apiUrl":          base + "/api",
		"downloadUrl":     base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":       base + "/upload",
		"eventSourceUrl":  base + "/events",
		"state":           "session-state",
	})
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
	case "AddressBook/get":
		return map[string]any{"accountId": testAccount, "state": "s0", "list": f.addressBooks}
	case "ContactCard/query":
		return map[string]any{"accountId": testAccount, "ids": f.cardIDs}
	case "ContactCard/get":
		var m cardGet
		_ = json.Unmarshal(call.Args, &m)
		list := []json.RawMessage{}
		for _, id := range m.IDs {
			if raw, ok := f.cards[id]; ok {
				list = append(list, raw)
			}
		}
		return map[string]any{"accountId": testAccount, "state": "s0", "list": list}
	case "ContactCard/changes":
		return map[string]any{
			"accountId": testAccount, "oldState": "old", "newState": f.changesNewState,
			"created": f.changesCreated, "updated": f.changesUpdated, "destroyed": f.changesDestroyed,
		}
	}
	return &gojmap.MethodError{Type: "unknownMethod"}
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// aliceCard is a JMAP ContactCard: a JSContact Card (RFC 9553) plus the JMAP id /
// addressBookIds members.
const aliceCard = `{
  "@type": "Card", "version": "1.0", "uid": "uid-alice",
  "id": "card-1", "addressBookIds": {"ab-1": true},
  "name": {"full": "Alice Smith", "components": [
    {"kind": "given", "value": "Alice"}, {"kind": "surname", "value": "Smith"}]},
  "organizations": {"o1": {"name": "Acme"}},
  "titles": {"t1": {"name": "Engineer"}},
  "emails": {"e1": {"address": "alice@example.com"}},
  "phones": {"p1": {"number": "+1-555-0100"}},
  "notes": {"n1": {"note": "VIP"}}
}`

func newFixture() *fakeServer {
	return &fakeServer{
		addressBooks: []*addressBook{{ID: "ab-1", Name: "Personal", Description: "Default book"}},
		cardIDs:      []gojmap.ID{"card-1"},
		cards:        map[gojmap.ID]json.RawMessage{"card-1": json.RawMessage(aliceCard)},
	}
}

func TestListAddressBooks(t *testing.T) {
	cl := newFixture().start(t)
	books, err := cl.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	if len(books) != 1 || books[0].ID != "ab-1" || books[0].Name != "Personal" {
		t.Fatalf("books = %+v", books)
	}
}

func TestListContactsMapsJSContact(t *testing.T) {
	cl := newFixture().start(t)
	cs, err := cl.ListContacts(context.Background(), "ab-1")
	if err != nil {
		t.Fatalf("ListContacts: %v", err)
	}
	if len(cs) != 1 {
		t.Fatalf("contact count = %d, want 1", len(cs))
	}
	c := cs[0]
	checks := map[string][2]string{
		"ID":            {c.ID, "card-1"},
		"AddressBookID": {c.AddressBookID, "ab-1"},
		"UID":           {c.UID, "uid-alice"},
		"DisplayName":   {c.DisplayName(), "Alice Smith"},
		"GivenName":     {c.GivenName(), "Alice"},
		"Surname":       {c.Surname(), "Smith"},
		"Organization":  {c.Organization(), "Acme"},
		"Title":         {c.Title(), "Engineer"},
		"Note":          {c.Note(), "VIP"},
	}
	for field, got := range checks {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", field, got[0], got[1])
		}
	}
	if emails := c.EmailList(); len(emails) != 1 || emails[0].Address != "alice@example.com" {
		t.Errorf("emails = %+v", emails)
	}
	if phones := c.PhoneList(); len(phones) != 1 || phones[0].Number != "+1-555-0100" {
		t.Errorf("phones = %+v", phones)
	}
}

func TestGetContact(t *testing.T) {
	cl := newFixture().start(t)
	c, err := cl.GetContact(context.Background(), "card-1")
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if c.UID != "uid-alice" || c.AddressBookID != "ab-1" {
		t.Errorf("contact = %+v", c)
	}
}

func TestGetContactNotFound(t *testing.T) {
	cl := newFixture().start(t)
	if _, err := cl.GetContact(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for a missing contact")
	}
}

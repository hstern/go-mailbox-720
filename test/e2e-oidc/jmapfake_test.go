// jmapfake_test.go is an in-process JMAP server for the impersonation e2e. It is
// the resource-server end of the chain: mailboxd, having exchanged a user's token
// for a JMAP-mail-audience token, dials this server with that opaque exchanged
// token as its bearer. The fake introspects the bearer via the same RFC 7662
// validator a real backend would (tokenValidator), resolves the subject, and
// serves that subject's mail out of the shared userStore — so a request for
// userA's data can never return userB's.
//
// It serves a JMAP session document advertising the mail capability + one
// account + an apiUrl pointing back at itself, and answers the three methods the
// mailjmap client issues to list mail: Mailbox/get (to find the inbox),
// Email/query (to list the inbox's emails), and Email/get (to fetch envelopes).
// The wire shapes are hand-built JSON maps rather than go-jmap structs, so the
// standalone e2e test module need not depend on go-jmap; the field names mirror
// the go-jmap response structs (RFC 8620/8621) the client decodes. The per-sub
// emails are synthesised from store.messages(sub) on each request, so the fake
// holds no per-user state of its own.
package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const (
	// jmapMailURI is the RFC 8621 mail capability URI the client looks up to
	// resolve the primary mail account.
	jmapMailURI = "urn:ietf:params:jmap:mail"
	// jmapCoreURI is the RFC 8620 core capability URI.
	jmapCoreURI = "urn:ietf:params:jmap:core"
	// jmapFakeAccount is the single account id the fake advertises and answers
	// for; the client resolves it as the primary mail account and echoes it back.
	jmapFakeAccount = "acct-1"
	// jmapFakeInbox is the id of the one mailbox the fake exposes (role inbox),
	// which the client selects when /me/messages passes an empty folder.
	jmapFakeInbox = "mbx-inbox"
)

// jmapFake is the stateless adapter: it validates each request's bearer with a
// tokenValidator and reads per-subject mail from the store.
type jmapFake struct {
	mailV *tokenValidator
	store *userStore
}

// startJMAPFake wires the fake into an httptest.Server and returns its session
// URL. mailV introspects the bearer for the JMAP-mail audience; contactsV is
// accepted for the Task 4 contacts slice and may be nil here (only mail is
// wired). store supplies per-subject seed data.
func startJMAPFake(t *testing.T, mailV, contactsV *tokenValidator, store *userStore) (sessionURL string) {
	t.Helper()
	_ = contactsV // contacts is wired in Task 4; mail-only here.
	f := &jmapFake{mailV: mailV, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/session", f.handleSession)
	mux.HandleFunc("/api", f.handleAPI)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/session"
}

// authenticate extracts the Authorization: Bearer token and introspects it via
// the validator, returning the subject. On any failure it writes 401 and reports
// false so the handler stops. The token itself is never logged.
func (f *jmapFake) authenticate(w http.ResponseWriter, r *http.Request) (sub string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return "", false
	}
	sub, err := f.mailV.validate(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
		return "", false
	}
	return sub, true
}

func (f *jmapFake) handleSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.authenticate(w, r); !ok {
		return
	}
	base := "http://" + r.Host
	session := map[string]any{
		"capabilities": map[string]any{
			jmapCoreURI: map[string]any{},
			jmapMailURI: map[string]any{},
		},
		"accounts": map[string]any{
			jmapFakeAccount: map[string]any{"name": "Mailbox", "isPersonal": true},
		},
		"primaryAccounts": map[string]any{
			jmapMailURI: jmapFakeAccount,
		},
		"username":    "mailbox",
		"apiUrl":      base + "/api",
		"downloadUrl": base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":   base + "/upload",
		"state":       "session-state",
	}
	writeJSONFake(w, session)
}

func (f *jmapFake) handleAPI(w http.ResponseWriter, r *http.Request) {
	sub, ok := f.authenticate(w, r)
	if !ok {
		return
	}
	var req jmapFakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	emails := f.emailsFor(sub)
	responses := make([]jmapInvocation, 0, len(req.Calls))
	for _, call := range req.Calls {
		name, args := f.dispatch(call, emails)
		responses = append(responses, jmapInvocation{name, args, call.CallID})
	}
	writeJSONFake(w, map[string]any{
		"methodResponses": responses,
		"sessionState":    "session-state",
	})
}

// fakeEmail is the envelope-level JMAP Email the list path needs (RFC 8621 field
// names). Only the properties /me/messages reads are populated.
type fakeEmail struct {
	ID         string          `json:"id"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Subject    string          `json:"subject"`
	From       []fakeAddress   `json:"from"`
	Keywords   map[string]bool `json:"keywords"`
}

type fakeAddress struct {
	Email string `json:"email"`
}

// emailsFor synthesises the subject's seeded messages as JMAP Email objects, all
// in the single inbox mailbox. Ids are positional ("email-0", …) and stable for
// one request, which is all the list path (query then get) needs.
func (f *jmapFake) emailsFor(sub string) map[string]fakeEmail {
	out := map[string]fakeEmail{}
	for i, m := range f.store.messages(sub) {
		id := "email-" + strconv.Itoa(i)
		out[id] = fakeEmail{
			ID:         id,
			MailboxIDs: map[string]bool{jmapFakeInbox: true},
			Subject:    m.Subject,
			From:       []fakeAddress{{Email: m.FromAddr}},
			Keywords:   map[string]bool{},
		}
	}
	return out
}

// dispatch answers a single method call against the subject's emails, returning
// the response method name and its args object.
func (f *jmapFake) dispatch(call jmapFakeCall, emails map[string]fakeEmail) (string, any) {
	switch call.Name {
	case "Mailbox/get":
		inbox := map[string]any{
			"id":           jmapFakeInbox,
			"name":         "Inbox",
			"role":         "inbox",
			"totalEmails":  len(emails),
			"unreadEmails": len(emails),
		}
		return call.Name, map[string]any{
			"accountId": jmapFakeAccount,
			"state":     "mbx-state",
			"list":      []any{inbox},
		}
	case "Email/query":
		// The list path filters inMailbox==inbox; every synthesised email is in
		// the inbox, so all of the subject's ids match.
		ids := make([]string, 0, len(emails))
		for id := range emails {
			ids = append(ids, id)
		}
		return call.Name, map[string]any{
			"accountId":  jmapFakeAccount,
			"queryState": "q0",
			"ids":        ids,
		}
	case "Email/get":
		var m struct {
			IDs []string `json:"ids"`
		}
		// A malformed args object yields an empty IDs slice (and an empty list),
		// which the test would catch loudly downstream; no need to fail the call.
		_ = json.Unmarshal(call.Args, &m)
		list := make([]fakeEmail, 0, len(m.IDs))
		notFound := []string{}
		for _, id := range m.IDs {
			if e, ok := emails[id]; ok {
				list = append(list, e)
			} else {
				notFound = append(notFound, id)
			}
		}
		return call.Name, map[string]any{
			"accountId": jmapFakeAccount,
			"state":     "e0",
			"list":      list,
			"notFound":  notFound,
		}
	}
	return "error", map[string]any{"type": "unknownMethod"}
}

// jmapInvocation marshals as the JMAP [name, args, callId] triple the client's
// Invocation decoder expects.
type jmapInvocation struct {
	Name   string
	Args   any
	CallID string
}

func (i jmapInvocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{i.Name, i.Args, i.CallID})
}

// jmapFakeRequest mirrors the JMAP request wire shape, keeping each call's args
// as raw JSON so the fake decodes the request args itself.
type jmapFakeRequest struct {
	Calls []jmapFakeCall `json:"methodCalls"`
}

type jmapFakeCall struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

func (c *jmapFakeCall) UnmarshalJSON(data []byte) error {
	var triple []json.RawMessage
	if err := json.Unmarshal(data, &triple); err != nil {
		return err
	}
	if len(triple) != 3 {
		return errBadJMAPCall
	}
	if err := json.Unmarshal(triple[0], &c.Name); err != nil {
		return err
	}
	c.Args = triple[1]
	return json.Unmarshal(triple[2], &c.CallID)
}

// errBadJMAPCall is returned when a method-call triple is malformed.
var errBadJMAPCall = jmapFakeErr("jmap fake: malformed method call")

type jmapFakeErr string

func (e jmapFakeErr) Error() string { return string(e) }

func writeJSONFake(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

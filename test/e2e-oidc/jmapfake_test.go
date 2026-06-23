// jmapfake_test.go is an in-process JMAP server for the impersonation e2e. It is
// the resource-server end of the chain: mailboxd, having exchanged a user's token
// for a JMAP-mail-audience token, dials this server with that opaque exchanged
// token as its bearer. The fake introspects the bearer via the same RFC 7662
// validator a real backend would (tokenValidator), resolves the subject, and
// serves that subject's mail out of the shared userStore — so a request for
// userA's data can never return userB's.
//
// It serves a JMAP session document advertising the mail and/or contacts
// capability + one account + an apiUrl pointing back at itself, and answers the
// methods each client issues. For mail (mailV != nil): Mailbox/get (to find the
// inbox), Email/query (to list the inbox's emails), and Email/get (to fetch
// envelopes). For contacts (contactsV != nil): AddressBook/get (to find the
// default book), ContactCard/query (to list the book's cards), and
// ContactCard/get (to fetch JSContact cards). The wire shapes are hand-built JSON
// maps rather than go-jmap structs, so the standalone e2e test module need not
// depend on go-jmap; the field names mirror the go-jmap response structs (RFC
// 8620/8621 for mail, RFC 9553/9610 for contacts) the clients decode. The per-sub
// data is synthesised from store.messages(sub) / store.contacts(sub) on each
// request, so the fake holds no per-user state of its own.
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
	// jmapContactsURI is the RFC 9610 contacts capability URI the contacts client
	// looks up to resolve the primary contacts account.
	jmapContactsURI = "urn:ietf:params:jmap:contacts"
	// jmapFakeBook is the id of the one address book the fake exposes, which the
	// contacts client selects as the principal's default book.
	jmapFakeBook = "ab-1"
)

// jmapFake is the stateless adapter: it validates each request's bearer with a
// tokenValidator and reads per-subject mail and/or contacts from the store. At
// least one of mailV/contactsV is set; each gates the matching capability,
// account, and methods. A nil validator means that protocol is not served.
type jmapFake struct {
	mailV     *tokenValidator
	contactsV *tokenValidator
	store     *userStore
}

// startJMAPFake wires the fake into an httptest.Server and returns its session
// URL. mailV introspects the bearer for the JMAP-mail audience and gates the mail
// capability + methods; contactsV does the same for the JMAP-contacts audience.
// Either may be nil to leave that protocol unwired. store supplies per-subject
// seed data.
func startJMAPFake(t *testing.T, mailV, contactsV *tokenValidator, store *userStore) (sessionURL string) {
	t.Helper()
	f := &jmapFake{mailV: mailV, contactsV: contactsV, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/session", f.handleSession)
	mux.HandleFunc("/api", f.handleAPI)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/session"
}

// authenticate extracts the Authorization: Bearer token and introspects it via
// each configured validator, returning the subject from the first that accepts
// it. Mail and contacts use distinct audiences, so a given bearer validates with
// exactly one validator; trying both lets a single server front either protocol.
// On any failure it writes 401 and reports false so the handler stops. The token
// itself is never logged.
func (f *jmapFake) authenticate(w http.ResponseWriter, r *http.Request) (sub string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		http.Error(w, "missing bearer", http.StatusUnauthorized)
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	var lastErr error
	for _, v := range []*tokenValidator{f.mailV, f.contactsV} {
		if v == nil {
			continue
		}
		if sub, err := v.validate(tok); err == nil {
			return sub, true
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errBadJMAPCall
	}
	http.Error(w, "invalid token: "+lastErr.Error(), http.StatusUnauthorized)
	return "", false
}

func (f *jmapFake) handleSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.authenticate(w, r); !ok {
		return
	}
	base := "http://" + r.Host
	capabilities := map[string]any{jmapCoreURI: map[string]any{}}
	primaryAccounts := map[string]any{}
	if f.mailV != nil {
		capabilities[jmapMailURI] = map[string]any{}
		primaryAccounts[jmapMailURI] = jmapFakeAccount
	}
	if f.contactsV != nil {
		capabilities[jmapContactsURI] = map[string]any{}
		primaryAccounts[jmapContactsURI] = jmapFakeAccount
	}
	session := map[string]any{
		"capabilities": capabilities,
		"accounts": map[string]any{
			jmapFakeAccount: map[string]any{"name": "Mailbox", "isPersonal": true},
		},
		"primaryAccounts": primaryAccounts,
		"username":        "mailbox",
		"apiUrl":          base + "/api",
		"downloadUrl":     base + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":       base + "/upload",
		"state":           "session-state",
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
	cards := f.cardsFor(sub)
	responses := make([]jmapInvocation, 0, len(req.Calls))
	for _, call := range req.Calls {
		name, args := f.dispatch(call, emails, cards)
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

// cardsFor synthesises the subject's seeded contacts as JMAP ContactCard objects
// (JSContact Cards, RFC 9553) in the single address book. Ids are positional
// ("card-0", …) and stable for one request, which is all the list path (query
// then get) needs. The display name is carried as the JSContact name.full member,
// which the server projects to Graph displayName.
func (f *jmapFake) cardsFor(sub string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for i, c := range f.store.contacts(sub) {
		id := "card-" + strconv.Itoa(i)
		out[id] = map[string]any{
			"@type":          "Card",
			"version":        "1.0",
			"uid":            "uid-" + id,
			"id":             id,
			"addressBookIds": map[string]bool{jmapFakeBook: true},
			"name":           map[string]any{"full": c.DisplayName},
		}
	}
	return out
}

// dispatch answers a single method call against the subject's emails or contacts,
// returning the response method name and its args object.
func (f *jmapFake) dispatch(call jmapFakeCall, emails map[string]fakeEmail, cards map[string]map[string]any) (string, any) {
	switch call.Name {
	case "AddressBook/get":
		book := map[string]any{
			"id":          jmapFakeBook,
			"name":        "Personal",
			"description": "Default book",
		}
		return call.Name, map[string]any{
			"accountId": jmapFakeAccount,
			"state":     "ab-state",
			"list":      []any{book},
		}
	case "ContactCard/query":
		// The list path filters inAddressBook==ab-1; every synthesised card is in
		// that book, so all of the subject's ids match.
		ids := make([]string, 0, len(cards))
		for id := range cards {
			ids = append(ids, id)
		}
		return call.Name, map[string]any{
			"accountId": jmapFakeAccount,
			"ids":       ids,
		}
	case "ContactCard/get":
		var m struct {
			IDs []string `json:"ids"`
		}
		_ = json.Unmarshal(call.Args, &m)
		list := make([]map[string]any, 0, len(m.IDs))
		notFound := []string{}
		for _, id := range m.IDs {
			if c, ok := cards[id]; ok {
				list = append(list, c)
			} else {
				notFound = append(notFound, id)
			}
		}
		return call.Name, map[string]any{
			"accountId": jmapFakeAccount,
			"state":     "cc-state",
			"list":      list,
			"notFound":  notFound,
		}
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

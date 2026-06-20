package carddav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gocarddav "github.com/emersion/go-webdav/carddav"
)

// go-webdav v0.7.0's in-process carddav.Handler does NOT implement the
// sync-collection REPORT: handleReport only dispatches addressbook-query and
// addressbook-multiget and rejects anything else with 400. So the read-path
// tests' Handler-backed server (newTestClient) cannot exercise a real
// sync-collection round-trip, and faking one through it would not test Delta's
// actual wire path.
//
// Instead, TestSyncDelta stands up a hand-rolled stub server that speaks the
// two requests Delta makes — the sync-collection REPORT and the follow-up GET
// of each changed object's vCard — and asserts that:
//   - Delta decodes the opaque address-book id to its collection path and
//     REPORTs against it,
//   - the supplied sync-token is passed through in the request body (empty for
//     initial sync, the prior token on the next call),
//   - each updated href is mapped to a Contact via the existing
//     contactFromObject/mapContact path and stamped with its opaque id, and
//   - the response's next sync-token is returned for the following call.
//
// The follow-up GET is necessary because go-webdav's SyncResponse.Updated
// objects never carry the vCard (only path/etag/last-modified); see Delta's doc
// comment.

const (
	deltaInitialToken = "" // initial sync sends an empty token
	deltaServerToken  = "http://example.com/sync/42"
)

// syncStubServer is an httptest server that answers Delta's two request kinds.
// It records the sync-token seen on the REPORT so the test can assert pass-
// through, and returns the seeded card on the follow-up GET.
type syncStubServer struct {
	t            *testing.T
	abPath       string
	cardPath     string
	cardBody     string
	nextToken    string
	gotSyncToken string // captured from the REPORT body
	reportCalls  int
	getCalls     int
}

func (s *syncStubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "REPORT" && r.URL.Path == s.abPath:
		s.reportCalls++
		body, _ := io.ReadAll(r.Body)
		s.gotSyncToken = extractSyncToken(string(body))
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		// A minimal RFC 6578 multistatus: one changed href plus the next
		// sync-token. address-data is intentionally omitted to mirror real
		// go-webdav behaviour, forcing Delta's follow-up GET.
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <response>
    <href>%s</href>
    <propstat>
      <prop><getetag>"etag-1"</getetag></prop>
      <status>HTTP/1.1 200 OK</status>
    </propstat>
  </response>
  <sync-token>%s</sync-token>
</multistatus>`, s.cardPath, s.nextToken)
	case r.Method == http.MethodGet && r.URL.Path == s.cardPath:
		s.getCalls++
		w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
		_, _ = io.WriteString(w, s.cardBody)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
	}
}

// extractSyncToken pulls the text between <sync-token> and </sync-token> out of
// the REPORT request body (which is small, hand-built XML).
func extractSyncToken(body string) string {
	const open, close = "<sync-token>", "</sync-token>"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// newSyncClient stands up the stub server and returns a Client pointed at it
// plus the stub so the test can inspect captured request state.
func newSyncClient(t *testing.T, stub *syncStubServer) (*Client, *syncStubServer) {
	t.Helper()
	stub.t = t
	ts := httptest.NewServer(stub)
	t.Cleanup(ts.Close)
	c, err := gocarddav.NewClient(nil, ts.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &Client{c: c}, stub
}

func TestSyncDelta(t *testing.T) {
	stub := &syncStubServer{
		abPath:    testAddressBook,
		cardPath:  testCardPath,
		cardBody:  serverCard,
		nextToken: deltaServerToken,
	}
	cl, stub := newSyncClient(t, stub)
	abID := addressBookID(testAddressBook)

	// Initial sync: empty token in, current contact + fresh token out.
	changed, next, err := cl.Delta(context.Background(), abID, deltaInitialToken)
	if err != nil {
		t.Fatalf("Delta(initial): %v", err)
	}
	if stub.gotSyncToken != deltaInitialToken {
		t.Errorf("REPORT sync-token = %q, want empty (initial sync)", stub.gotSyncToken)
	}
	if next != deltaServerToken {
		t.Errorf("next token = %q, want %q", next, deltaServerToken)
	}
	if len(changed) != 1 {
		t.Fatalf("got %d changed contacts, want 1", len(changed))
	}
	c := changed[0]
	if c.ID != contactID(testCardPath) {
		t.Errorf("ID = %q, want %q", c.ID, contactID(testCardPath))
	}
	if c.AddressBookID != abID {
		t.Errorf("AddressBookID = %q, want %q", c.AddressBookID, abID)
	}
	if c.DisplayName != "Alice Gopher" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName, "Alice Gopher")
	}
	if c.UID != "alice-uid-1" {
		t.Errorf("UID = %q, want %q", c.UID, "alice-uid-1")
	}
	if stub.reportCalls != 1 || stub.getCalls != 1 {
		t.Errorf("calls: REPORT=%d GET=%d, want 1 and 1", stub.reportCalls, stub.getCalls)
	}

	// Next call: feed the returned token back; assert it reaches the server.
	if _, _, err := cl.Delta(context.Background(), abID, next); err != nil {
		t.Fatalf("Delta(next): %v", err)
	}
	if stub.gotSyncToken != deltaServerToken {
		t.Errorf("second REPORT sync-token = %q, want %q (fed back)", stub.gotSyncToken, deltaServerToken)
	}
}

func TestSyncDeltaInvalidAddressBookID(t *testing.T) {
	stub := &syncStubServer{abPath: testAddressBook, cardPath: testCardPath, cardBody: serverCard}
	cl, stub := newSyncClient(t, stub)
	if _, _, err := cl.Delta(context.Background(), "!!!", ""); err == nil {
		t.Error("Delta(invalid id) = nil error, want error")
	}
	if stub.reportCalls != 0 {
		t.Errorf("REPORT was called %d times for an invalid id; want 0 (decode must fail first)", stub.reportCalls)
	}
}

// impersonation_jmap_test.go is the MVP slice that proves the whole MB720-52
// impersonation chain end to end for the first time: a real Zitadel mints a user
// token, a real mailboxd subprocess validates it at the front door and RFC 8693-
// exchanges it (sub-preserving) for a JMAP-mail-audience token, and an in-process
// JMAP fake introspects that exchanged token and serves only that subject's mail.
// Two users assert no cross-tenant bleed.
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationJMAPMail(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedMessages(z.subjectFor(t, userA), message{Subject: "A inbox", FromAddr: "a@example.com"})
	store.seedMessages(z.subjectFor(t, userB), message{Subject: "B inbox", FromAddr: "b@example.com"})

	mailV := newTokenValidator(t, z, z.backendAudience(t, audMailJMAP))
	sessionURL := startJMAPFake(t, mailV, nil, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-mail-jmap-session-url", sessionURL,
		"-mail-jmap-audience", z.backendAudience(t, audMailJMAP),
	})

	// userA sees only A's inbox.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userA), "A inbox")
	// userB sees only B's inbox — no cross-tenant bleed.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userB), "B inbox")
}

// assertSingleMessageSubject asserts that GET /me/messages with the given user
// token returns exactly one message with the wanted subject. Defined here and
// reused by the later protocol slices (Task 7).
func assertSingleMessageSubject(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/messages", token)
	if code != http.StatusOK {
		t.Fatalf("/me/messages: %d %s", code, body)
	}
	var resp struct {
		Value []struct {
			Subject string `json:"subject"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(resp.Value) != 1 || resp.Value[0].Subject != want {
		t.Fatalf("messages = %s, want exactly [%q]", body, want)
	}
}

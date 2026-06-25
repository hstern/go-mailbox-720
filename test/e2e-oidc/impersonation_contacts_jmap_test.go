// impersonation_contacts_jmap_test.go proves the MB720-52 impersonation chain for
// the contacts-over-JMAP per-identity path: a real Zitadel mints a user token, a
// real mailboxd subprocess validates it at the front door and RFC 8693-exchanges
// it (sub-preserving) for a JMAP-contacts-audience token, and the in-process JMAP
// fake introspects that exchanged token and serves only that subject's contacts.
// Two users assert no cross-tenant bleed.
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationContactsJMAP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedContacts(z.subjectFor(t, userA), contact{DisplayName: "A Contact"})
	store.seedContacts(z.subjectFor(t, userB), contact{DisplayName: "B Contact"})

	contactsV := newTokenValidator(t, z, z.backendAudience(t, audContactsJMAP))
	sessionURL := startJMAPFake(t, nil, contactsV, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-contacts-jmap-session-url", sessionURL,
		"-contacts-jmap-audience", z.backendAudience(t, audContactsJMAP),
	})

	// userA sees only A's contact.
	assertSingleContact(t, base, z.mintUserToken(t, userA), "A Contact")
	// userB sees only B's contact — no cross-tenant bleed.
	assertSingleContact(t, base, z.mintUserToken(t, userB), "B Contact")
}

// assertSingleContact asserts that GET /me/contacts with the given user token
// returns exactly one contact with the wanted display name. Defined here and
// reused by the later CardDAV slice (Task 6).
func assertSingleContact(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/contacts", token)
	if code != http.StatusOK {
		t.Fatalf("/me/contacts: %d %s", code, body)
	}
	var resp struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(resp.Value) != 1 || resp.Value[0].DisplayName != want {
		t.Fatalf("contacts = %s, want exactly [%q]", body, want)
	}
}

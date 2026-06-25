// impersonation_dav_test.go proves the MB720-52 impersonation chain for the
// calendar-over-CalDAV per-identity path: a real Zitadel mints a user token, a
// real mailboxd subprocess validates it at the front door and RFC 8693-exchanges
// it (sub-preserving) for a CalDAV-audience token, and the in-process WebDAV fake
// introspects that exchanged token and serves only that subject's calendar
// events. Two users assert no cross-tenant bleed.
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationCalDAV(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedEvents(z.subjectFor(t, userA), event{Subject: "A meeting"})
	store.seedEvents(z.subjectFor(t, userB), event{Subject: "B meeting"})

	v := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	url := startCalDAVFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-cal-caldav-url", url,
		"-cal-caldav-audience", z.backendAudience(t, audCalDAV),
	})

	// userA sees only A's event.
	assertSingleEvent(t, base, z.mintUserToken(t, userA), "A meeting")
	// userB sees only B's event — no cross-tenant bleed.
	assertSingleEvent(t, base, z.mintUserToken(t, userB), "B meeting")
}

// TestImpersonationCardDAV proves the same chain for the contacts-over-CardDAV
// per-identity path: mailboxd exchanges the user's token for a CardDAV-audience
// token and the in-process WebDAV fake (the CardDAV face of startCardDAVFake)
// introspects it and serves only that subject's contacts. Two users assert no
// cross-tenant bleed.
func TestImpersonationCardDAV(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedContacts(z.subjectFor(t, userA), contact{DisplayName: "A Card"})
	store.seedContacts(z.subjectFor(t, userB), contact{DisplayName: "B Card"})

	v := newTokenValidator(t, z, z.backendAudience(t, audCardDAV))
	url := startCardDAVFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-contacts-carddav-url", url,
		"-contacts-carddav-audience", z.backendAudience(t, audCardDAV),
	})

	// userA sees only A's contact.
	assertSingleContact(t, base, z.mintUserToken(t, userA), "A Card")
	// userB sees only B's contact — no cross-tenant bleed.
	assertSingleContact(t, base, z.mintUserToken(t, userB), "B Card")
}

// assertSingleEvent asserts that GET /me/events with the given user token returns
// exactly one event with the wanted subject. Defined here and reused by the later
// slice (Task 8).
func assertSingleEvent(t *testing.T, base, token, want string) {
	t.Helper()
	code, body := get(t, base+"/me/events", token)
	if code != http.StatusOK {
		t.Fatalf("/me/events: %d %s", code, body)
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
		t.Fatalf("events = %s, want exactly [%q]", body, want)
	}
}

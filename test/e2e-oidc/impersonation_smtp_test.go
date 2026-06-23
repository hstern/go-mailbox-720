// impersonation_smtp_test.go proves the MB720-52 impersonation chain for the
// outbound iMIP-over-SMTP per-identity path: a real Zitadel mints a user token, a
// real mailboxd subprocess validates it at the front door and RFC 8693-exchanges
// it (sub-preserving) for an SMTP-audience token, and the in-process SMTP fake
// authenticates that exchanged token via SASL OAUTHBEARER, resolves the subject,
// and records the message it receives under that subject.
//
// The SMTP path is exercised by accepting a meeting invite: mailboxd reads the
// invite (a seeded CalDAV event carrying an ORGANIZER) and, because the CalDAV
// fake does no RFC 6638 server-side scheduling, emails a METHOD:REPLY iMIP message
// itself — from the authenticated subject's address, over the per-identity SMTP
// backend. The test asserts a reply was recorded under userA's subject.
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestImpersonationSMTP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	calV := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	calURL := startCalDAVFake(t, calV, store) // accept-meeting needs a calendar backend
	smtpV := newTokenValidator(t, z, z.backendAudience(t, audSMTP))
	smtpAddr := startSMTPFake(t, smtpV, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-cal-caldav-url", calURL, "-cal-caldav-audience", z.backendAudience(t, audCalDAV),
		"-smtp-addr", smtpAddr, "-smtp-audience", z.backendAudience(t, audSMTP),
		"-smtp-tls=false", "-smtp-starttls=false",
	})

	acceptSeededInvite(t, base, z, store, userA)
	if got := store.sent(z.subjectFor(t, userA)); len(got) == 0 {
		t.Fatal("no iMIP reply recorded for userA")
	}
}

// acceptSeededInvite seeds an invitation event (carrying an ORGANIZER, so a reply
// has someone to go to) for the given user in the CalDAV fake, reads it back via
// GET /me/events to learn the opaque event id mailboxd assigned, and POSTs the
// accept that mailboxd turns into a METHOD:REPLY iMIP send over the per-identity
// SMTP backend. The CalDAV fake does no RFC 6638 scheduling, so mailboxd emails
// the reply itself rather than delegating to the server.
func acceptSeededInvite(t *testing.T, base string, z *zitadelIDP, store *userStore, user string) {
	t.Helper()
	sub := z.subjectFor(t, user)
	store.seedEvents(sub, event{Subject: "Invite for " + user, Organizer: "organizer@example.com"})

	tok := z.mintUserToken(t, user)

	// Read the event back to learn the id mailboxd assigns (an opaque encoding of
	// the CalDAV object path), then accept that id.
	code, body := get(t, base+"/me/events", tok)
	if code != http.StatusOK {
		t.Fatalf("/me/events: %d %s", code, body)
	}
	var resp struct {
		Value []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode events: %v (%s)", err, body)
	}
	if len(resp.Value) != 1 {
		t.Fatalf("want exactly one seeded event, got %s", body)
	}
	id := resp.Value[0].ID
	if id == "" {
		t.Fatalf("seeded event has no id: %s", body)
	}

	// SendResponse defaults to true; omit it so mailboxd emails the organizer.
	acode, abody := doJSON(t, http.MethodPost, base+"/me/events/"+id+"/accept", tok,
		`{"@odata.type":"#microsoft.graph.event"}`)
	if acode != http.StatusOK && acode != http.StatusNoContent {
		t.Fatalf("POST accept: %d %s", acode, abody)
	}
}

// impersonation_imap_test.go proves the MB720-52 impersonation chain through the
// IMAP backend: a real Zitadel mints a user token, a real mailboxd subprocess
// validates it at the front door and RFC 8693-exchanges it (sub-preserving) for
// an IMAP-backend-audience token, and an in-process IMAP fake authenticates that
// exchanged token via SASL OAUTHBEARER (introspecting it) and serves only that
// subject's INBOX. Two users assert no cross-tenant bleed.
package e2e

import "testing"

func TestImpersonationIMAP(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedMessages(z.subjectFor(t, userA), message{Subject: "A imap", FromAddr: "a@example.com"})
	store.seedMessages(z.subjectFor(t, userB), message{Subject: "B imap", FromAddr: "b@example.com"})

	v := newTokenValidator(t, z, z.backendAudience(t, audIMAP))
	addr := startIMAPFake(t, v, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-mail-imap-addr", addr,
		"-mail-imap-tls=false",
		"-mail-imap-audience", z.backendAudience(t, audIMAP),
	})

	// userA sees only A's inbox.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userA), "A imap")
	// userB sees only B's inbox — no cross-tenant bleed.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userB), "B imap")
}

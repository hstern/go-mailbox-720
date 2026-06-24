// impersonation_jmap_jwt_test.go proves the requested_token_type=jwt path
// (MB720-56) end to end. It mirrors TestImpersonationJMAPMail but flips two
// things: mailboxd runs with -tokenexchange-requested-token-type=...:jwt, so
// Zitadel issues the exchanged backend token as a JWS JWT (not the opaque JWE it
// returns for the default :access_token); and the JMAP fake validates that token
// OFFLINE against the issuer's JWKS (jwksValidator) instead of via an RFC 7662
// introspection round-trip (tokenValidator). That offline validation succeeding
// is the whole point of the flag — backends fronted by a JWT-issuing AS no longer
// pay an introspection call per request.
package e2e

import "testing"

func TestImpersonationRequestedTokenTypeJWT(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	store := newUserStore()
	store.seedMessages(z.subjectFor(t, userA), message{Subject: "A inbox", FromAddr: "a@example.com"})
	store.seedMessages(z.subjectFor(t, userB), message{Subject: "B inbox", FromAddr: "b@example.com"})

	// The backend validates the exchanged JWT offline via the issuer's JWKS — no
	// introspection. This only works because the exchanged token is now a JWS JWT.
	mailV := newJWKSValidator(t, z, z.backendAudience(t, audMailJMAP))
	sessionURL := startJMAPFake(t, mailV, nil, store)

	base := startMailboxdImpersonation(t, z, []string{
		"-mail-jmap-session-url", sessionURL,
		"-mail-jmap-audience", z.backendAudience(t, audMailJMAP),
		"-tokenexchange-requested-token-type", "urn:ietf:params:oauth:token-type:jwt",
	})

	// userA sees only A's inbox; userB only B's — same isolation as the opaque-token
	// slice, now proven with offline JWKS validation of a JWT exchanged token.
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userA), "A inbox")
	assertSingleMessageSubject(t, base, z.mintUserToken(t, userB), "B inbox")
}

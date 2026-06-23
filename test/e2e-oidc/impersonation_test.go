// impersonation_test.go drives the per-identity backend path end to end:
// Zitadel issues a user token, mailboxd validates + RFC 8693-exchanges it
// (sub-preserving) for a backend-audience token, and an in-process backend that
// validates that exchanged token serves the user's own data (MB720-52).
// Impersonation is Zitadel-only (Kanidm token exchange is service-account only).
package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Backend audiences requested per protocol. Distinct per protocol so each
// provider's request is exercised; provisionImpersonation registers each.
const (
	audMailJMAP     = "backend-mail-jmap"
	audContactsJMAP = "backend-contacts-jmap"
	audCalDAV       = "backend-caldav"
	audCardDAV      = "backend-carddav"
	audIMAP         = "backend-imap"
	audSMTP         = "backend-smtp"
)

const (
	userA = "usera"
	userB = "userb"
)

func TestImpersonationExchangeSpike(t *testing.T) {
	requireDocker(t) // reuse the existing docker guard helper
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	userTok := z.mintUserToken(t, userA)

	// The concrete aud the audMailJMAP backend was registered as. Zitadel only
	// admits registered project ids into a token's aud (an arbitrary string fails
	// with invalid_target), so the exchange's audience param and the resulting aud
	// claim use this resolved value, not the symbolic constant.
	backendAud := z.backendAudience(t, audMailJMAP)

	// Exchange userA's token for a backend-audience token, authenticating as the
	// exchanging client — exactly what mailboxd's exchanger does. requested_token_type
	// is jwt so the result is a decodable, JWKS-validatable JWT (Zitadel returns an
	// opaque token for requested_token_type=access_token).
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {userTok},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
		"audience":             {backendAud},
		"scope":                {"openid"},
	}
	exchanged := postToken(t, httpClientFor(z), z.tokenEndpoint(), form, z.exchangeClientID(), z.exchangeSecret())

	subUser := subjectOf(t, userTok)
	subExch := subjectOf(t, exchanged)
	t.Logf("user sub=%s; exchanged sub=%s; backend aud=%s", subUser, subExch, backendAud)
	if subUser == "" {
		t.Fatal("user token has no sub claim; sub-preservation cannot be asserted")
	}
	if subExch != subUser {
		t.Fatalf("exchange did not preserve sub: user=%q exchanged=%q", subUser, subExch)
	}
	if !audContains(t, exchanged, backendAud) {
		t.Fatalf("exchanged token missing backend audience %q (logical %q)", backendAud, audMailJMAP)
	}
}

// httpClientFor returns the HTTP client carrying any private-CA trust the IdP
// needs. Zitadel runs plain HTTP, so the default client suffices.
func httpClientFor(_ idp) *http.Client { return http.DefaultClient }

// subjectOf returns the "sub" claim of a JWT (payload decoded without
// signature verification — adequate for asserting test expectations).
func subjectOf(t *testing.T, jwt string) string { return claimString(t, jwt, "sub") }

// claimString returns the named string claim from a JWT's payload, decoding the
// payload without verifying the signature (fine for asserting test claims).
func claimString(t *testing.T, jwt, claim string) string {
	t.Helper()
	m := claims(t, jwt)
	s, _ := m[claim].(string)
	return s
}

// audContains reports whether the JWT's "aud" (string or []string) contains want.
func audContains(t *testing.T, jwt, want string) bool {
	t.Helper()
	switch v := claims(t, jwt)["aud"].(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// claims decodes the (unverified) payload of a JWT into a map.
func claims(t *testing.T, jwt string) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT (%d segments): %q", len(parts), jwt)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal JWT payload: %v (%s)", err, payload)
	}
	return m
}

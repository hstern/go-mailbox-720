// tokenvalidator_test.go is the shared RFC 7662 introspection validator the
// impersonation e2e backends use to trust an exchanged token. Production mailboxd
// hardcodes requested_token_type=access_token (internal/tokenexchange/
// tokenexchange.go:148), and Zitadel returns an encrypted JWE for that request —
// opaque to JWKS but introspectable. So a backend cannot validate the exchanged
// token by decoding a JWT; it must introspect (RFC 7662) and check that the token
// carries ITS backend audience (a resolved Zitadel project id). This mirrors the
// project memory kanidm-opaque-tokens-need-introspection.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// tokenValidator introspects a bearer token (RFC 7662) and accepts it only if it is
// active AND carries a specific backend audience (a resolved Zitadel project id).
// Each backend protocol constructs one for its own audience so it trusts a token
// exchanged for IT, not any Zitadel token.
type tokenValidator struct {
	endpoint   string // introspection endpoint
	clientID   string // HTTP Basic client id authorising introspection
	secret     string // HTTP Basic client secret
	backendAud string // required backend audience (a Zitadel project id)
	client     *http.Client
}

// newTokenValidator captures the introspection endpoint + client creds + the
// required backend audience (a resolved project id from z.backendAudience).
func newTokenValidator(t *testing.T, z *zitadelIDP, backendAud string) *tokenValidator {
	t.Helper()
	return &tokenValidator{
		endpoint:   z.introspectionEndpoint(),
		clientID:   z.backendIntrospectClientID(t, backendAud),
		secret:     z.backendIntrospectSecret(t, backendAud),
		backendAud: backendAud,
		client:     httpClientFor(z),
	}
}

// validate POSTs token=<bearer> to the introspection endpoint (HTTP Basic), decodes
// the RFC 7662 response, and returns the sub only when the token is active AND
// carries the required backend audience. Zitadel surfaces a project audience both in
// the "aud" array and as the reserved scope token
// urn:zitadel:iam:org:project:id:<projectID>:aud, so both are checked. The token is
// opaque (an encrypted JWE), so this never decodes a JWT — stdlib only.
func (v *tokenValidator) validate(bearer string) (sub string, err error) {
	req, err := http.NewRequest(http.MethodPost, v.endpoint,
		strings.NewReader(url.Values{"token": {bearer}}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(v.clientID, v.secret)

	resp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("introspection returned %d: %s", resp.StatusCode, body)
	}

	var ir struct {
		Active bool   `json:"active"`
		Sub    string `json:"sub"`
		Scope  string `json:"scope"`
		Aud    any    `json:"aud"` // string or []string
	}
	if err := json.Unmarshal(body, &ir); err != nil {
		return "", fmt.Errorf("decode introspection response: %w", err)
	}
	if !ir.Active {
		return "", fmt.Errorf("token is not active")
	}
	if !audInIntrospection(ir.Aud, ir.Scope, v.backendAud) {
		return "", fmt.Errorf("token lacks required backend audience %q", v.backendAud)
	}
	return ir.Sub, nil
}

// audInIntrospection reports whether want appears as a backend audience in an
// RFC 7662 response — either directly in aud (string or []string) or as the reserved
// Zitadel project-audience scope token urn:zitadel:iam:org:project:id:<want>:aud.
func audInIntrospection(aud any, scope, want string) bool {
	switch v := aud.(type) {
	case string:
		if v == want {
			return true
		}
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	projAud := "urn:zitadel:iam:org:project:id:" + want + ":aud"
	for _, s := range strings.Fields(scope) {
		if s == projAud {
			return true
		}
	}
	return false
}

func TestTokenValidatorChecksBackendAudience(t *testing.T) {
	requireDocker(t)
	z := &zitadelIDP{}
	z.start(t)
	z.provision(t)
	z.provisionImpersonation(t)

	// A token exchanged for the CalDAV backend audience must validate for a CalDAV
	// validator (returning the user's sub) and be REJECTED by a JMAP-mail validator
	// — this audience check is what makes the backend "trust the exchanged token"
	// for ITS protocol rather than any Zitadel token.
	userTok := z.mintUserToken(t, userA)
	calTok := z.exchangeForBackend(t, userTok, z.backendAudience(t, audCalDAV))

	calV := newTokenValidator(t, z, z.backendAudience(t, audCalDAV))
	sub, err := calV.validate(calTok)
	if err != nil {
		t.Fatalf("CalDAV validator rejected a CalDAV-audience token: %v", err)
	}
	if want := z.subjectFor(t, userA); sub != want {
		t.Fatalf("validated sub = %q, want %q", sub, want)
	}

	mailV := newTokenValidator(t, z, z.backendAudience(t, audMailJMAP))
	if _, err := mailV.validate(calTok); err == nil {
		t.Fatal("JMAP-mail validator accepted a CalDAV-audience token")
	}
}

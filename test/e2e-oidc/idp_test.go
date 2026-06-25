// idp.go defines the IdP matrix the e2e runs against. Each identity provider
// (Kanidm, Zitadel, …) implements the idp interface; the vertical-slice tests run
// against every entry returned by idps(). Optional capabilities (e.g. RFC 8693
// user impersonation) are declared per IdP so capability-specific tests skip the
// providers that don't support them — Kanidm's token exchange is service-account
// only, so it is excluded from impersonation testing (MB720-15 / MB720-41).
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// idpCaps declares an IdP's optional capabilities.
type idpCaps struct {
	// impersonation is true when the IdP can perform RFC 8693 token exchange that
	// impersonates an end user (subject_token of a user, result preserves the
	// user's sub). Kanidm cannot; Zitadel can.
	impersonation bool
}

// idp is an OAuth2/OIDC identity provider the e2e stands up in a container and
// provisions a "mailbox" resource server against. It supplies everything mailboxd
// needs to enforce auth, plus a way to mint a mailbox access token.
type idp interface {
	// name is the subtest label (e.g. "kanidm", "zitadel").
	name() string
	// start boots the IdP container(s); it returns once OIDC discovery is reachable.
	start(t *testing.T)
	// provision registers the mailbox resource-server client/scope/user.
	provision(t *testing.T)
	// mintToken returns a mailbox access token carrying a subject.
	mintToken(t *testing.T) string

	// The following configure mailboxd's auth middleware.
	issuer() string             // -auth-issuer (OIDC issuer URL)
	audience() string           // -auth-audience
	scope() string              // -auth-scope ("" = require none)
	introspectClientID() string // -auth-introspect-client-id ("" = JWT/JWKS only)
	introspectSecret() string   // MAILBOXD_INTROSPECT_CLIENT_SECRET ("" = none)
	sslCertFile() string        // SSL_CERT_FILE for a private CA ("" = system roots / http)

	caps() idpCaps
}

// impersonator is the optional capability the impersonation e2e needs from an IdP:
// it provisions RFC 8693 end-user impersonation, mints a named user's (subject)
// token, and exposes the token endpoint + the OAuth client mailboxd authenticates
// the exchange with.
type impersonator interface {
	provisionImpersonation(t *testing.T)
	mintUserToken(t *testing.T, user string) string
	tokenEndpoint() string
	exchangeClientID() string
	exchangeSecret() string
}

// Zitadel is the impersonation-capable IdP.
var _ impersonator = (*zitadelIDP)(nil)

// idps is the IdP matrix. Baseline vertical-slice tests run against every entry.
func idps() []idp {
	return []idp{
		&kanidmIDP{},
		&zitadelIDP{},
	}
}

// authFlags builds the mailboxd auth command-line flags for an IdP.
func authFlags(p idp) []string {
	f := []string{
		"-auth-issuer", p.issuer(),
		"-auth-audience", p.audience(),
	}
	if s := p.scope(); s != "" {
		f = append(f, "-auth-scope", s)
	}
	if c := p.introspectClientID(); c != "" {
		f = append(f, "-auth-introspect-client-id", c)
	}
	return f
}

// authEnv builds the mailboxd auth environment for an IdP (secret + optional CA).
func authEnv(p idp) []string {
	var e []string
	if f := p.sslCertFile(); f != "" {
		e = append(e, "SSL_CERT_FILE="+f)
	}
	if s := p.introspectSecret(); s != "" {
		e = append(e, "MAILBOXD_INTROSPECT_CLIENT_SECRET="+s)
	}
	return e
}

// postToken POSTs an OAuth2 token request (form-encoded). When basicID is set the
// client authenticates with HTTP Basic; otherwise credentials are expected in the
// form. The provided client carries any private-CA trust the IdP needs.
func postToken(t *testing.T, client *http.Client, tokenURL string, form url.Values, basicID, basicSecret string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicID != "" {
		req.SetBasicAuth(basicID, basicSecret)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint (%s) returned %d: %s", form.Get("grant_type"), resp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatalf("decode token response: %v (%s)", err, body)
	}
	if tok.AccessToken == "" {
		t.Fatalf("empty access_token in: %s", body)
	}
	return tok.AccessToken
}

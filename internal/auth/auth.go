// Package auth provides the OIDC resource-server middleware for the mailbox
// server. We validate bearer tokens issued by one or more external IdPs (BYO:
// Keycloak / Authentik / Dex / Entra / Kanidm); we are NOT an issuer and expose
// no /authorize or /token endpoints of our own.
//
// Each configured issuer is discovered via .well-known/openid-configuration at
// New time. Two token shapes are supported:
//
//   - JWT access tokens are validated locally: signature against the issuer's
//     JWKS (fetched + cached + refreshed by go-oidc), issuer, audience, and
//     expiry/not-before.
//   - Opaque access tokens (e.g. Kanidm's default) are validated via RFC 7662
//     token introspection against the issuer's introspection endpoint, using the
//     resource server's own client credentials.
//
// Either way the middleware enforces the required scopes, maps a configurable
// claim to the mailbox identity ("me"), and stashes it in the request context.
//
// Reusing Henry's go-oidc-federation / go-authzen is a future option; this is the
// minimal resource-server validation built directly on coreos/go-oidc.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/grapherr"
)

// Config configures the resource-server middleware.
type Config struct {
	// Issuers are the trusted IdP issuer URLs. Each is discovered at New time.
	Issuers []string
	// Audience is the expected token aud (the resource we represent). Empty skips
	// the audience check (not recommended outside local testing).
	Audience string
	// RequiredScopes must all be present in the token's scp/scope or roles.
	RequiredScopes []string
	// SubjectClaim names the claim mapped to the mailbox identity ("me"), e.g.
	// "sub", "oid", or "preferred_username". Defaults to "sub".
	SubjectClaim string
	// Introspection, when set, enables RFC 7662 validation of opaque (non-JWT)
	// tokens against each issuer's introspection endpoint.
	Introspection *IntrospectionConfig
}

// IntrospectionConfig holds the resource server's own OAuth2 client credentials,
// used to authenticate to the issuer's introspection endpoint.
type IntrospectionConfig struct {
	ClientID     string
	ClientSecret string
}

// Authenticator validates bearer tokens against the configured issuers.
type Authenticator struct {
	verifiers      map[string]*oidc.IDTokenVerifier // JWT validation, keyed by issuer
	introspectURLs map[string]string                // RFC 7662 endpoints, keyed by issuer
	introspect     *IntrospectionConfig
	httpClient     *http.Client
	audience       string
	requiredScopes []string
	subjectClaim   string
}

// New discovers each configured issuer and builds the Authenticator. It errors
// if no issuers are configured or any issuer fails discovery (fail-closed).
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if len(cfg.Issuers) == 0 {
		return nil, fmt.Errorf("auth: no issuers configured")
	}
	subjectClaim := cfg.SubjectClaim
	if subjectClaim == "" {
		subjectClaim = "sub"
	}
	a := &Authenticator{
		verifiers:      make(map[string]*oidc.IDTokenVerifier, len(cfg.Issuers)),
		introspectURLs: make(map[string]string),
		introspect:     cfg.Introspection,
		httpClient:     http.DefaultClient,
		audience:       cfg.Audience,
		requiredScopes: cfg.RequiredScopes,
		subjectClaim:   subjectClaim,
	}
	for _, iss := range cfg.Issuers {
		provider, err := oidc.NewProvider(ctx, iss)
		if err != nil {
			return nil, fmt.Errorf("auth: discover issuer %q: %w", iss, err)
		}
		a.verifiers[iss] = provider.Verifier(&oidc.Config{
			ClientID:          cfg.Audience,
			SkipClientIDCheck: cfg.Audience == "",
		})
		if cfg.Introspection != nil {
			var meta struct {
				IntrospectionEndpoint string `json:"introspection_endpoint"`
			}
			if err := provider.Claims(&meta); err == nil && meta.IntrospectionEndpoint != "" {
				a.introspectURLs[iss] = meta.IntrospectionEndpoint
			}
		}
	}
	if cfg.Introspection != nil && len(a.introspectURLs) == 0 {
		return nil, fmt.Errorf("auth: introspection configured but no issuer advertises introspection_endpoint")
	}
	return a, nil
}

// Middleware wraps next, rejecting any request without a valid bearer token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		claims, issuer, err := a.validate(r.Context(), raw)
		if err != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		if !claims.hasScopes(a.requiredScopes) {
			grapherr.Write(w, http.StatusForbidden)
			return
		}
		// The mailbox identity is the RFC 9493 (issuer, subject) pair: a bare
		// subject is unique only within its issuer, so scoping it to the issuer
		// keeps mailboxes distinct (and unspoofable) across multiple BYO IdPs.
		mailbox := subjectid.IssSubID{Iss: issuer, Sub: claims.string(a.subjectClaim)}
		if mailbox.Validate() != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withMailbox(r.Context(), mailbox)))
	})
}

// validate resolves a bearer token to its claims. A JWT from a known issuer is
// validated locally against that issuer's JWKS; anything else (an opaque token,
// or a JWT whose issuer we only introspect) is validated via RFC 7662.
func (a *Authenticator) validate(ctx context.Context, raw string) (claims claimSet, issuer string, err error) {
	if looksLikeJWT(raw) {
		if iss, err := unverifiedIssuer(raw); err == nil {
			if verifier, ok := a.verifiers[iss]; ok {
				tok, err := verifier.Verify(ctx, raw)
				if err != nil {
					return nil, "", err
				}
				var claims claimSet
				if err := tok.Claims(&claims); err != nil {
					return nil, "", err
				}
				return claims, iss, nil
			}
		}
	}
	if len(a.introspectURLs) > 0 {
		return a.introspectToken(ctx, raw)
	}
	return nil, "", fmt.Errorf("auth: no validator for the presented token")
}

// introspectToken validates an opaque token against the configured introspection
// endpoints, returning the claims and issuer of the first that reports it active.
// Opaque tokens carry no readable issuer, so each endpoint is tried in turn.
func (a *Authenticator) introspectToken(ctx context.Context, raw string) (claimSet, string, error) {
	for iss, endpoint := range a.introspectURLs {
		claims, err := introspect(ctx, a.httpClient, endpoint, a.introspect.ClientID, a.introspect.ClientSecret, raw)
		if err != nil {
			continue
		}
		if active, _ := claims["active"].(bool); active && claims.audienceAllows(a.audience) {
			return claims, iss, nil
		}
	}
	return nil, "", fmt.Errorf("auth: token not active for this resource per introspection")
}

// introspect performs an RFC 7662 introspection request, authenticating with the
// resource server's client credentials.
func introspect(ctx context.Context, client *http.Client, endpoint, clientID, clientSecret, token string) (claimSet, error) {
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(clientID, clientSecret)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("introspection endpoint returned %s", resp.Status)
	}
	var claims claimSet
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, fmt.Errorf("decode introspection response: %w", err)
	}
	return claims, nil
}

// looksLikeJWT reports whether raw has the three dot-separated segments of a JWS.
func looksLikeJWT(raw string) bool {
	return strings.Count(raw, ".") == 2
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// unverifiedIssuer reads the "iss" claim from a JWT WITHOUT verifying it, purely
// to select the matching verifier. The selected verifier performs full
// validation, so this is not a trust decision.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode jwt payload: %w", err)
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse jwt payload: %w", err)
	}
	if claims.Iss == "" {
		return "", fmt.Errorf("jwt missing iss")
	}
	return claims.Iss, nil
}

// claimSet is the decoded token claims (from a JWT or an introspection response),
// kept generic so the mailbox-identity claim can be configured.
type claimSet map[string]any

// scopes gathers the union of granted scopes from scp/scope (space-delimited
// strings or arrays) and roles (arrays).
func (c claimSet) scopes() map[string]struct{} {
	set := map[string]struct{}{}
	add := func(s string) {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	for _, key := range []string{"scp", "scope"} {
		switch v := c[key].(type) {
		case string:
			for s := range strings.FieldsSeq(v) {
				add(s)
			}
		case []any:
			for _, s := range v {
				if str, ok := s.(string); ok {
					add(str)
				}
			}
		}
	}
	if roles, ok := c["roles"].([]any); ok {
		for _, s := range roles {
			if str, ok := s.(string); ok {
				add(str)
			}
		}
	}
	return set
}

// audienceAllows reports whether the token may be used for want. The JWT path
// has the verifier enforce aud; for introspected tokens we check the response's
// aud member when present (string or array). When aud is absent — as with
// Kanidm, whose introspection omits it — the call is gated only by the resource
// server's own introspection credentials, which is the best available binding.
func (c claimSet) audienceAllows(want string) bool {
	if want == "" {
		return true
	}
	switch v := c["aud"].(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
		return false
	default:
		return true // aud absent or unrecognized shape: cannot check here.
	}
}

// hasScopes reports whether every required scope is present.
func (c claimSet) hasScopes(required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := c.scopes()
	for _, r := range required {
		if _, ok := have[r]; !ok {
			return false
		}
	}
	return true
}

// string returns claim key as a string, or "" if absent or not a string.
func (c claimSet) string(key string) string {
	s, _ := c[key].(string)
	return s
}

type ctxKey struct{}

func withMailbox(ctx context.Context, id subjectid.SubjectIdentifier) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Mailbox returns the authenticated mailbox identity the middleware stored — an
// RFC 9493 Subject Identifier (an IssSubID) — and whether the request was
// authenticated.
func Mailbox(ctx context.Context) (subjectid.SubjectIdentifier, bool) {
	id, ok := ctx.Value(ctxKey{}).(subjectid.SubjectIdentifier)
	return id, ok
}

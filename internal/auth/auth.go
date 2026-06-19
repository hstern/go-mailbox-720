// Package auth provides the OIDC resource-server middleware for the mailbox
// server. We validate bearer JWTs issued by one or more external IdPs (BYO:
// Keycloak / Authentik / Dex / Entra / Kanidm); we are NOT an issuer and expose
// no /authorize or /token endpoints of our own.
//
// Each configured issuer is discovered via .well-known/openid-configuration at
// New time; its JWKS is fetched and cached (refreshed on demand) by go-oidc. On
// every request the middleware validates the token's signature, issuer, audience
// and expiry/not-before, enforces the required scopes, maps a configurable claim
// to the mailbox identity ("me"), and stashes it in the request context.
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
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

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
}

// Authenticator validates bearer JWTs against the configured issuers.
type Authenticator struct {
	verifiers      map[string]*oidc.IDTokenVerifier // keyed by issuer URL
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
		// Route to the verifier by the token's (unverified) issuer; the verifier
		// then re-checks iss against its provider and validates the signature, so
		// a forged iss can only select a verifier that will reject the token.
		iss, err := unverifiedIssuer(raw)
		if err != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		verifier, ok := a.verifiers[iss]
		if !ok {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		tok, err := verifier.Verify(r.Context(), raw)
		if err != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		var claims claimSet
		if err := tok.Claims(&claims); err != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		if !claims.hasScopes(a.requiredScopes) {
			grapherr.Write(w, http.StatusForbidden)
			return
		}
		mailbox := claims.string(a.subjectClaim)
		if mailbox == "" {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withMailbox(r.Context(), mailbox)))
	})
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

// claimSet is the decoded JWT claims, kept generic so the mailbox-identity claim
// can be configured.
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

func withMailbox(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// Mailbox returns the authenticated mailbox identity the middleware stored, and
// whether a request was authenticated.
func Mailbox(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

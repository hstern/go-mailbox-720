// Package auth provides the OIDC resource-server middleware for the mailbox
// server. We validate bearer tokens issued by one or more external IdPs (BYO:
// Keycloak / Authentik / Dex / Entra / Kanidm); we are NOT an issuer and expose
// no /authorize or /token endpoints of our own.
//
// Each configured issuer is discovered via .well-known/openid-configuration at
// New time. Two token shapes are supported:
//
//   - JWT access tokens: the JWS is verified against the issuer's JWKS (signature,
//     algorithm pinning, issuer, audience, expiry) by go-oidc, then the verified
//     payload is validated against the RFC 9068 "JWT Profile for OAuth 2.0 Access
//     Tokens" claim set by go-access-tokens. RFC 9068 §2.2 requires iss, sub, aud,
//     exp, iat, jti, and client_id — so a JWT lacking jti or client_id is rejected
//     on this path. (Opaque tokens are unaffected.)
//   - Opaque access tokens (e.g. Kanidm's default) are validated via RFC 7662 token
//     introspection (go-token-introspection) against the issuer's introspection
//     endpoint, using the resource server's own client credentials.
//
// Either way the middleware enforces the required scopes, maps a configurable
// claim to the mailbox identity ("me"), and stashes it in the request context.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	accesstoken "github.com/hstern/go-access-tokens"
	subjectid "github.com/hstern/go-subjectid"
	introspection "github.com/hstern/go-token-introspection"

	"github.com/hstern/go-mailbox-720/internal/grapherr"
)

// Config configures the resource-server middleware.
type Config struct {
	// Issuers are the trusted IdP issuer URLs. Each is discovered at New time.
	Issuers []string
	// Audience is the expected token aud (the resource we represent). Empty skips
	// the audience check (not recommended outside local testing).
	Audience string
	// RequiredScopes must all be present in the token's scope or roles.
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
	verifiers      map[string]*oidc.IDTokenVerifier // JWS verification, keyed by issuer
	introspectors  map[string]*introspection.Client // RFC 7662 clients, keyed by issuer
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
		introspectors:  make(map[string]*introspection.Client),
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
				a.introspectors[iss] = introspection.NewClient(meta.IntrospectionEndpoint,
					introspection.WithBasicAuth(cfg.Introspection.ClientID, cfg.Introspection.ClientSecret),
					introspection.WithHTTPClient(http.DefaultClient))
			}
		}
	}
	if cfg.Introspection != nil && len(a.introspectors) == 0 {
		return nil, fmt.Errorf("auth: introspection configured but no issuer advertises introspection_endpoint")
	}
	return a, nil
}

// principal is the validated, backend-neutral result of either token path: the
// issuer that vouched for the subject and the granted scopes. Both the JWT and the
// introspection path produce one, so Middleware stays uniform.
type principal struct {
	issuer  string
	subject string
	scopes  map[string]struct{}
}

// hasScopes reports whether every required scope is present.
func (p *principal) hasScopes(required []string) bool {
	for _, r := range required {
		if _, ok := p.scopes[r]; !ok {
			return false
		}
	}
	return true
}

// Middleware wraps next, rejecting any request without a valid bearer token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		p, err := a.validate(r.Context(), raw)
		if err != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		if !p.hasScopes(a.requiredScopes) {
			grapherr.Write(w, http.StatusForbidden)
			return
		}
		// The mailbox identity is the RFC 9493 (issuer, subject) pair: a bare
		// subject is unique only within its issuer, so scoping it to the issuer
		// keeps mailboxes distinct (and unspoofable) across multiple BYO IdPs.
		mailbox := subjectid.IssSubID{Iss: p.issuer, Sub: p.subject}
		if mailbox.Validate() != nil {
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withMailbox(r.Context(), mailbox)))
	})
}

// validate resolves a bearer token to a principal. A JWT from a known issuer is
// validated locally against that issuer's JWKS + the RFC 9068 profile; anything
// else (an opaque token, or a JWT whose issuer we only introspect) is validated
// via RFC 7662.
func (a *Authenticator) validate(ctx context.Context, raw string) (*principal, error) {
	if looksLikeJWT(raw) {
		if iss, err := unverifiedIssuer(raw); err == nil {
			if verifier, ok := a.verifiers[iss]; ok {
				return a.validateJWT(ctx, verifier, iss, raw)
			}
		}
	}
	if len(a.introspectors) > 0 {
		return a.introspectToken(ctx, raw)
	}
	return nil, fmt.Errorf("auth: no validator for the presented token")
}

// validateJWT verifies the token's JWS — signature, the issuer's discovery-pinned
// algorithms, issuer, audience, and expiry, via go-oidc — then validates the RFC
// 9068 access-token claim profile (go-access-tokens) over the now-verified payload.
// Keeping go-oidc for the crypto preserves algorithm pinning; go-access-tokens adds
// the §2.2 required-claim and §4 checks an ID-token verifier does not make.
func (a *Authenticator) validateJWT(ctx context.Context, verifier *oidc.IDTokenVerifier, iss, raw string) (*principal, error) {
	if _, err := verifier.Verify(ctx, raw); err != nil {
		return nil, err
	}
	// The signature is verified; decode the (now trusted) payload for the RFC 9068
	// validator rather than re-running the crypto.
	payload, err := jwtPayload(raw)
	if err != nil {
		return nil, err
	}
	claims, err := accesstoken.ParseClaims(payload)
	if err != nil {
		return nil, fmt.Errorf("auth: parse access token: %w", err)
	}
	opts := []accesstoken.Option{accesstoken.WithIssuer(iss)}
	if a.audience != "" {
		opts = append(opts, accesstoken.WithAudience(a.audience))
	}
	if err := claims.Validate(opts...); err != nil {
		return nil, fmt.Errorf("auth: validate access token: %w", err)
	}
	return &principal{
		issuer:  iss,
		subject: subjectFromClaims(claims, a.subjectClaim),
		scopes:  toSet(append(claims.ScopeValues(), claims.Roles...)),
	}, nil
}

// introspectToken validates an opaque token against the configured introspection
// endpoints, returning the principal of the first that reports it active and
// audience-bound. Opaque tokens carry no readable issuer, so each is tried in turn.
func (a *Authenticator) introspectToken(ctx context.Context, raw string) (*principal, error) {
	for iss, client := range a.introspectors {
		resp, err := client.Introspect(ctx, &introspection.Request{
			Token:         raw,
			TokenTypeHint: introspection.TokenTypeHintAccessToken,
		})
		if err != nil {
			// A transport/auth failure at the endpoint is a misconfiguration, not a
			// normal "inactive" answer — surface it (then try the next issuer).
			log.Printf("auth: introspection at issuer %q failed: %v", iss, err)
			continue
		}
		if !resp.Active {
			continue
		}
		if !audienceAllows(resp.Audience, a.audience) {
			continue
		}
		return &principal{
			issuer:  iss,
			subject: subjectFromResponse(resp, a.subjectClaim),
			scopes:  toSet(resp.Scopes()),
		}, nil
	}
	return nil, fmt.Errorf("auth: token not active for this resource per introspection")
}

// subjectFromClaims extracts the configured mailbox-identity claim from a validated
// RFC 9068 claim set: a typed field for the registered claims, else an extension
// claim (oid, preferred_username, …) via GetExtra.
func subjectFromClaims(c *accesstoken.Claims, claim string) string {
	switch claim {
	case "", "sub":
		return c.Subject
	case "client_id":
		return c.ClientID
	case "iss":
		return c.Issuer
	case "jti":
		return c.JWTID
	default:
		var s string
		if ok, _ := c.GetExtra(claim, &s); ok {
			return s
		}
		return ""
	}
}

// subjectFromResponse extracts the configured mailbox-identity claim from an RFC
// 7662 introspection response, mirroring subjectFromClaims.
func subjectFromResponse(r *introspection.Response, claim string) string {
	switch claim {
	case "", "sub":
		return r.Subject
	case "client_id":
		return r.ClientID
	case "iss":
		return r.Issuer
	case "username":
		return r.Username
	default:
		var s string
		if ok, _ := r.GetExtra(claim, &s); ok {
			return s
		}
		return ""
	}
}

// audienceAllows reports whether an introspected token may be used here. An empty
// want skips the check. A response that omits aud is gated only by the resource
// server's own introspection credentials — some IdPs (e.g. Kanidm) omit aud and
// refuse to introspect another client's token at all; a present aud must name this
// resource server.
func audienceAllows(aud introspection.Audience, want string) bool {
	if want == "" || len(aud) == 0 {
		return true
	}
	return aud.Contains(want)
}

// toSet collects non-empty strings into a set.
func toSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, s := range items {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

// looksLikeJWT reports whether raw has the three dot-separated segments of a JWS.
func looksLikeJWT(raw string) bool {
	return strings.Count(raw, ".") == 2
}

// jwtPayload returns the decoded JSON payload (the second segment) of a compact
// JWS. It is only called after the signature has been verified, so the bytes are
// trusted; it lets the RFC 9068 validator run on the verified payload without
// re-doing signature crypto.
func jwtPayload(raw string) ([]byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("auth: malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("auth: decode jwt payload: %w", err)
	}
	return payload, nil
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

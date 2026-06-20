// Package auth provides the OIDC resource-server middleware for the mailbox
// server. We validate bearer tokens issued by one or more external IdPs (BYO:
// Keycloak / Authentik / Dex / Entra / Kanidm); we are NOT an issuer and expose
// no /authorize or /token endpoints of our own.
//
// Each configured issuer is discovered via .well-known/openid-configuration at
// New time. Two token shapes are supported:
//
//   - JWT bearer tokens: the JWS is verified against the issuer's JWKS — signature,
//     the issuer's discovery-pinned algorithms, issuer, audience, expiry — by
//     go-oidc. That is the access-control gate. A token that declares itself an RFC
//     9068 "JWT Profile for OAuth 2.0 Access Tokens" (typ=at+jwt) is additionally
//     held to that profile by go-access-tokens (the §2.2 required claims incl. jti
//     and client_id, and the §4 checks); a plain typ=JWT token — as Microsoft Entra
//     and many IdPs issue — is accepted on signature + audience like any bearer JWT.
//     The token's typ thus selects how strictly it is decoded.
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
	bearer "github.com/hstern/go-bearer-token"
	prm "github.com/hstern/go-protected-resource-metadata"
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
	// ScopeClaims names the claims that carry granted scopes; each value (a
	// space-delimited string or a JSON array of strings) is unioned for the
	// RequiredScopes check. Defaults to ["scope", "roles"] — the standardized scope
	// claim (RFC 8693 §4.2, used by RFC 9068 §2.2.3) plus app roles.
	//
	// Microsoft Entra / Azure AD does not use the standard claim: it carries
	// delegated permissions in a non-standard "scp" claim (and app permissions in
	// "roles"). "scp" is not RFC- or IANA-registered, so it is not read by default;
	// an Entra-fronted deployment should set ScopeClaims to ["scope", "scp", "roles"].
	ScopeClaims []string
	// Introspection, when set, enables RFC 7662 validation of opaque (non-JWT)
	// tokens against each issuer's introspection endpoint.
	Introspection *IntrospectionConfig
	// ResourceID is this resource's identifier URL (RFC 8707), published as the
	// "resource" member of the RFC 9728 protected-resource metadata. When non-empty,
	// the server serves that metadata at /.well-known/oauth-protected-resource so
	// clients can discover the authorization servers and scopes.
	ResourceID string
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
	scopeClaims    []string
	// resourceMetadataURL, when set, is this resource's RFC 9728 metadata URL,
	// added to the WWW-Authenticate challenge as the §5.1 resource_metadata param.
	resourceMetadataURL string
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
	scopeClaims := cfg.ScopeClaims
	if len(scopeClaims) == 0 {
		scopeClaims = []string{"scope", "roles"}
	}
	a := &Authenticator{
		verifiers:      make(map[string]*oidc.IDTokenVerifier, len(cfg.Issuers)),
		introspectors:  make(map[string]*introspection.Client),
		audience:       cfg.Audience,
		requiredScopes: cfg.RequiredScopes,
		subjectClaim:   subjectClaim,
		scopeClaims:    scopeClaims,
	}
	if cfg.ResourceID != "" {
		// Best-effort: a malformed resource id just omits the §5.1 link; the
		// metadata endpoint (auth.MetadataEndpoint) surfaces the error at mount.
		if url, err := prm.WellKnownPath(cfg.ResourceID); err == nil {
			a.resourceMetadataURL = url
		}
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
		raw, err := bearer.Token(r) // RFC 6750 §2.1 bearer extraction
		if err != nil {
			a.challenge(w, "") // no credentials: a bare challenge, no error (RFC 6750 §3)
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		p, err := a.validate(r.Context(), raw)
		if err != nil {
			a.challenge(w, bearer.ErrorInvalidToken)
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		if !p.hasScopes(a.requiredScopes) {
			a.challenge(w, bearer.ErrorInsufficientScope)
			grapherr.Write(w, http.StatusForbidden)
			return
		}
		// The mailbox identity is the RFC 9493 (issuer, subject) pair: a bare
		// subject is unique only within its issuer, so scoping it to the issuer
		// keeps mailboxes distinct (and unspoofable) across multiple BYO IdPs.
		mailbox := subjectid.IssSubID{Iss: p.issuer, Sub: p.subject}
		if mailbox.Validate() != nil {
			a.challenge(w, bearer.ErrorInvalidToken)
			grapherr.Write(w, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withMailbox(r.Context(), mailbox)))
	})
}

// challenge sets the RFC 6750 §3 "WWW-Authenticate: Bearer" header on an auth
// failure, so a client learns how to authenticate (and why it was refused). errCode
// is "" for a request carrying no credentials — the spec omits the error code then —
// else bearer.ErrorInvalidToken or bearer.ErrorInsufficientScope (with the required
// scope). We use Challenge.SetHeader (header only), not Respond, so grapherr.Write
// keeps owning the status and the Graph error body.
func (a *Authenticator) challenge(w http.ResponseWriter, errCode string) {
	c := bearer.Challenge{Realm: a.audience, Error: errCode}
	if errCode == bearer.ErrorInsufficientScope {
		c.Scope = strings.Join(a.requiredScopes, " ")
	}
	if a.resourceMetadataURL != "" {
		// RFC 9728 §5.1: point the client at our protected-resource metadata.
		name, value := prm.ChallengeParam(a.resourceMetadataURL)
		c.Extra = map[string]string{name: value}
	}
	c.SetHeader(w)
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
// algorithms, issuer, audience, and expiry, via go-oidc (the access-control gate
// for every JWT, and what preserves algorithm pinning) — then decodes the verified
// token for typed claim access. A token that declares itself an RFC 9068 access
// token (typ=at+jwt) is additionally held to that profile; a plain JWT is not, so
// non-RFC-9068 issuers (Microsoft Entra, …) work without being rejected for a
// missing jti/client_id.
func (a *Authenticator) validateJWT(ctx context.Context, verifier *oidc.IDTokenVerifier, iss, raw string) (*principal, error) {
	if _, err := verifier.Verify(ctx, raw); err != nil {
		return nil, err
	}
	// The signature is verified; decode the same (now trusted) compact token for
	// typed claim access rather than re-running the crypto.
	tok, err := accesstoken.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("auth: parse token: %w", err)
	}
	if accesstoken.ValidType(tok.Header.Type) {
		// The token claims to be an RFC 9068 access token: hold it to the profile.
		opts := []accesstoken.Option{accesstoken.WithIssuer(iss)}
		if a.audience != "" {
			opts = append(opts, accesstoken.WithAudience(a.audience))
		}
		if err := tok.Validate(opts...); err != nil {
			return nil, fmt.Errorf("auth: validate access token: %w", err)
		}
	}
	return &principal{
		issuer:  iss,
		subject: subjectFromClaims(&tok.Claims, a.subjectClaim),
		scopes:  toSet(scopesFromClaims(&tok.Claims, a.scopeClaims)),
	}, nil
}

// scopesFromClaims gathers the granted scopes from the configured claims of a JWT
// access token, unioning their values. The standard "scope" and the typed RFC 9068
// list claims use their typed accessors; any other configured claim (e.g. Entra's
// "scp") is read from the extension claims.
func scopesFromClaims(c *accesstoken.Claims, claimNames []string) []string {
	var out []string
	for _, name := range claimNames {
		switch name {
		case "scope":
			out = append(out, c.ScopeValues()...)
		case "roles":
			out = append(out, c.Roles...)
		case "groups":
			out = append(out, c.Groups...)
		case "entitlements":
			out = append(out, c.Entitlements...)
		default:
			out = append(out, extraScopes(c.GetExtra, name)...)
		}
	}
	return out
}

// scopesFromResponse is the introspection-side analog of scopesFromClaims: RFC 7662
// standardizes only "scope" (typed here), so any other configured claim is read
// from the response's extension members.
func scopesFromResponse(r *introspection.Response, claimNames []string) []string {
	var out []string
	for _, name := range claimNames {
		if name == "scope" {
			out = append(out, r.Scopes()...)
			continue
		}
		out = append(out, extraScopes(r.GetExtra, name)...)
	}
	return out
}

// extraScopes reads a non-typed scope-bearing claim via the claim set's GetExtra,
// accepting either a space-delimited string (e.g. Entra's "scp") or a JSON array of
// strings. A claim that is absent — or present but neither shape — contributes none.
func extraScopes(getExtra func(string, any) (bool, error), name string) []string {
	var s string
	if ok, err := getExtra(name, &s); ok && err == nil {
		return strings.Fields(s)
	}
	var arr []string
	if ok, err := getExtra(name, &arr); ok && err == nil {
		return arr
	}
	return nil
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
			scopes:  toSet(scopesFromResponse(resp, a.scopeClaims)),
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

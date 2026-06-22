// Package tokenexchange mints a backend-audience access token from the validated
// incoming user token, so a per-identity backend provider can authenticate to a
// backend as the user who authenticated to mailboxd.
//
// The incoming bearer token is audience-bound to mailboxd (the RFC 9068
// resource-server model), so a backend that checks aud rejects it verbatim. The
// Exchanger performs an RFC 8693 token exchange (grant_type=token-exchange) that
// trades the user's token for a fresh one scoped to the backend's audience while
// preserving the user's sub — impersonation, not pass-through. mailboxd is the
// authenticated OAuth client of that exchange; the client authentication travels
// on the *http.Client handed to New (e.g. one built by go-oauth-client-authn),
// which this package does not interpret.
//
// Exchanged tokens are cached keyed by (subject token, audience) so repeated
// requests from one user to one backend reuse one exchanged token instead of
// re-exchanging each time. The cache key is a SHA-256 of the subject token
// (never the token itself) plus the audience; an entry is served until a
// configurable skew before its advertised expiry, and a token the AS gives no
// lifetime is never cached (we cannot know when it dies, so correctness wins over
// caching). The cache prevents steady-state re-exchange; it does not coalesce
// concurrent cold-cache misses for the same key (each performs its own exchange
// until one populates the entry) — a singleflight is a possible later refinement.
//
// This package deliberately does NOT import internal/auth: it takes the raw
// subject token as a string, so the auth middleware and the exchange seam stay
// decoupled (MB720-41 foundation; consumed per-protocol by MB720-42/43/44).
package tokenexchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	rfc8693 "github.com/hstern/go-token-exchange"
)

// defaultSkew is how long before a token's advertised expiry the cache stops
// serving it, leaving headroom for clock skew and in-flight use of the result.
const defaultSkew = 30 * time.Second

// Token is the result of an exchange: a backend-audience access token and when it
// expires. ExpiresAt is zero when the authorization server advertised no
// lifetime (the token is then returned but never cached).
type Token struct {
	// AccessToken is the issued backend-audience token (RFC 8693 §2.2.1).
	AccessToken string
	// TokenType is the OAuth 2.0 token_type, typically "Bearer" (RFC 6749 §7.1).
	TokenType string
	// ExpiresAt is the token's expiry, or the zero time when the AS gave none.
	ExpiresAt time.Time
}

// Exchanger trades a validated incoming user token for a backend-audience token
// that preserves the user's identity. Implementations are safe for concurrent
// use.
type Exchanger interface {
	// Exchange returns a token scoped to audience, minted from subjectToken via
	// RFC 8693 token exchange. A cached, unexpired result is returned without
	// contacting the authorization server.
	Exchange(ctx context.Context, subjectToken, audience string) (Token, error)
}

// Option configures an Exchanger at construction time.
type Option func(*cachingExchanger)

// WithClock overrides the time source (default time.Now). Intended for tests.
func WithClock(now func() time.Time) Option {
	return func(c *cachingExchanger) {
		if now != nil {
			c.now = now
		}
	}
}

// WithSkew overrides how long before expiry a cached token stops being served
// (default 30s). A non-positive value resets to the default.
func WithSkew(d time.Duration) Option {
	return func(c *cachingExchanger) {
		if d > 0 {
			c.skew = d
		}
	}
}

// New returns a caching Exchanger that performs RFC 8693 token exchange against
// the authorization server at tokenEndpoint. httpClient carries mailboxd's OAuth
// client authentication to that endpoint (e.g. from go-oauth-client-authn); a nil
// httpClient uses the go-token-exchange default (no client authentication).
func New(tokenEndpoint string, httpClient *http.Client, opts ...Option) Exchanger {
	client := rfc8693.NewClient(tokenEndpoint, rfc8693.WithHTTPClient(httpClient))
	return newCaching(client, opts...)
}

// cachingExchanger wraps a go-token-exchange Client with a per-(subject,audience)
// token cache. The cache has no eviction: entries persist for the process
// lifetime, which suits the current deployment; a long-running multi-tenant
// deployment wants a bounded cache, noted as follow-up (mirrors revocation.Store).
type cachingExchanger struct {
	client rfc8693.Client
	skew   time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]Token
}

// newCaching builds a cachingExchanger over an already-constructed Client. It is
// the seam tests use to inject a stub Client.
func newCaching(client rfc8693.Client, opts ...Option) *cachingExchanger {
	c := &cachingExchanger{
		client: client,
		skew:   defaultSkew,
		now:    time.Now,
		cache:  make(map[string]Token),
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

func (c *cachingExchanger) Exchange(ctx context.Context, subjectToken, audience string) (Token, error) {
	if subjectToken == "" {
		return Token{}, fmt.Errorf("tokenexchange: empty subject token")
	}
	key := cacheKey(subjectToken, audience)
	now := c.now()

	c.mu.Lock()
	if tok, ok := c.cache[key]; ok && now.Before(tok.ExpiresAt.Add(-c.skew)) {
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	// Miss: exchange outside the lock so concurrent exchanges for different keys
	// do not serialize (and a slow AS does not block cache reads).
	resp, err := c.client.Exchange(ctx, &rfc8693.TokenExchangeRequest{
		GrantType:          rfc8693.GrantTypeTokenExchange,
		SubjectToken:       subjectToken,
		SubjectTokenType:   rfc8693.TokenTypeAccessToken,
		RequestedTokenType: rfc8693.TokenTypeAccessToken,
		Audience:           []string{audience},
	})
	if err != nil {
		return Token{}, fmt.Errorf("tokenexchange: exchange: %w", err)
	}
	if err := resp.Validate(); err != nil {
		return Token{}, fmt.Errorf("tokenexchange: invalid response: %w", err)
	}

	tok := Token{AccessToken: resp.AccessToken, TokenType: resp.TokenType}
	// Cache only when the AS advertised a positive lifetime; otherwise we cannot
	// know when the token dies, so it is returned but re-exchanged next time.
	if resp.ExpiresIn > 0 {
		tok.ExpiresAt = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
		c.mu.Lock()
		c.cache[key] = tok
		c.mu.Unlock()
	}
	return tok, nil
}

// cacheKey is a SHA-256 over the subject token and audience, with a separator so
// distinct (subject, audience) splits cannot collide. The subject token is
// hashed, never stored, so the cache holds no bearer tokens as map keys.
func cacheKey(subjectToken, audience string) string {
	h := sha256.New()
	h.Write([]byte(subjectToken))
	h.Write([]byte{0})
	h.Write([]byte(audience))
	return hex.EncodeToString(h.Sum(nil))
}

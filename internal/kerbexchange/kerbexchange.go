// Package kerbexchange mints Kerberos credentials for the authenticated end user
// from their validated incoming access token, so a per-identity backend provider
// can authenticate to a Kerberos/GSSAPI-only backend (Dovecot/Cyrus GSSAPI SASL,
// SPNEGO/Negotiate-fronted DAV) as the user who authenticated to mailboxd.
//
// This is the Kerberos counterpart of internal/tokenexchange. Where that package
// trades the user's OAuth token for a backend-audience OAuth token (RFC 8693),
// this one calls a go-oauth2-kerberos-exchange service — itself an RFC 8693
// profile — that validates the user's token and issues a Kerberos credential for
// that user: an MIT krb5 ccache or a ready-made GSSAPI/SPNEGO AP-REQ, capped to
// the token's expiry. No master-user, no stored passwords (MB720-45 foundation;
// consumed per-protocol by MB720-46 IMAP/SMTP SASL GSSAPI, MB720-47 CalDAV/CardDAV
// SPNEGO, MB720-48 JMAP SPNEGO).
//
// Credentials are cached keyed by (subject token, target SPN, output type) so
// repeated requests from one user to one backend reuse one credential instead of
// re-minting each time. The cache key is a SHA-256 of the subject token (never the
// token itself) plus the target and output; an entry is served until a
// configurable skew before its expiry, and a credential with no expiry is returned
// but never cached (we cannot know when it dies, so correctness wins over caching).
// The cache prevents steady-state re-minting; it does not coalesce concurrent
// cold-cache misses for the same key (each performs its own exchange until one
// populates the entry) — a singleflight is a possible later refinement.
//
// This package deliberately does NOT import internal/auth: it takes the raw
// subject token as a string, so the auth middleware and the exchange seam stay
// decoupled. Any client authentication mailboxd must present to the exchange
// endpoint rides on the *http.Client handed to New (the endpoint authenticates the
// subject token itself, so none is required by default).
package kerbexchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	"github.com/hstern/go-oauth2-kerberos-exchange/httpexchange"
)

// defaultSkew is how long before a credential's expiry the cache stops serving
// it, leaving headroom for clock skew and in-flight use of the result.
const defaultSkew = 30 * time.Second

// Exchanger trades a validated incoming user token for a Kerberos credential that
// authenticates as that user to the target service principal. Implementations are
// safe for concurrent use.
type Exchanger interface {
	// Exchange returns a Kerberos credential for target in the requested output
	// shape, minted from subjectToken by the exchange service. A cached, unexpired
	// credential is returned without contacting the service.
	Exchange(ctx context.Context, subjectToken string, target kerb.ServicePrincipal, output kerb.OutputType) (*kerb.Credential, error)
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

// WithSkew overrides how long before expiry a cached credential stops being
// served (default 30s). A non-positive value resets to the default.
func WithSkew(d time.Duration) Option {
	return func(c *cachingExchanger) {
		if d > 0 {
			c.skew = d
		}
	}
}

// New returns a caching Exchanger that mints Kerberos credentials via the
// go-oauth2-kerberos-exchange service at tokenEndpoint. httpClient carries any
// transport configuration mailboxd needs to reach that endpoint (TLS roots,
// client authentication); a nil httpClient uses http.DefaultClient.
func New(tokenEndpoint string, httpClient *http.Client, opts ...Option) Exchanger {
	return newCaching(httpexchange.NewClient(tokenEndpoint, httpClient), opts...)
}

// client is the subset of httpexchange.Client this package depends on. It is the
// seam tests use to inject a stub; *httpexchange.Client satisfies it.
type client interface {
	Exchange(ctx context.Context, accessToken string, target kerb.ServicePrincipal, output kerb.OutputType) (*kerb.Credential, error)
}

// cachingExchanger wraps an exchange client with a per-(subject, target, output)
// credential cache. The cache has no eviction: entries persist for the process
// lifetime, which suits the current deployment; a long-running multi-tenant
// deployment wants a bounded cache, noted as follow-up (mirrors revocation.Store).
type cachingExchanger struct {
	client client
	skew   time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]*kerb.Credential
}

// newCaching builds a cachingExchanger over an already-constructed client. It is
// the seam tests use to inject a stub client.
func newCaching(c client, opts ...Option) *cachingExchanger {
	ce := &cachingExchanger{
		client: c,
		skew:   defaultSkew,
		now:    time.Now,
		cache:  make(map[string]*kerb.Credential),
	}
	for _, o := range opts {
		if o != nil {
			o(ce)
		}
	}
	return ce
}

func (c *cachingExchanger) Exchange(ctx context.Context, subjectToken string, target kerb.ServicePrincipal, output kerb.OutputType) (*kerb.Credential, error) {
	if subjectToken == "" {
		return nil, fmt.Errorf("kerbexchange: empty subject token")
	}
	key := cacheKey(subjectToken, target, output)
	now := c.now()

	c.mu.Lock()
	if cred, ok := c.cache[key]; ok && now.Before(cred.Expiry().Add(-c.skew)) {
		c.mu.Unlock()
		return cred, nil
	}
	c.mu.Unlock()

	// Miss: exchange outside the lock so concurrent exchanges for different keys
	// do not serialize (and a slow service does not block cache reads).
	cred, err := c.client.Exchange(ctx, subjectToken, target, output)
	if err != nil {
		return nil, fmt.Errorf("kerbexchange: exchange: %w", err)
	}
	if cred == nil {
		return nil, fmt.Errorf("kerbexchange: exchange returned no credential")
	}

	// Cache only when the credential has a known expiry; one without cannot be
	// invalidated on time, so it is returned but re-minted next time.
	if !cred.Expiry().IsZero() {
		c.mu.Lock()
		c.cache[key] = cred
		c.mu.Unlock()
	}
	return cred, nil
}

// cacheKey is a SHA-256 over the subject token, target SPN, and output type, with
// separators so distinct splits cannot collide. The subject token is hashed,
// never stored, so the cache holds no bearer tokens as map keys.
func cacheKey(subjectToken string, target kerb.ServicePrincipal, output kerb.OutputType) string {
	h := sha256.New()
	h.Write([]byte(subjectToken))
	h.Write([]byte{0})
	h.Write([]byte(target.String()))
	h.Write([]byte{0})
	h.Write([]byte(output.String()))
	return hex.EncodeToString(h.Sum(nil))
}

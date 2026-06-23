package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/auth"
	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/smtp"
	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

// identityBackendSkew is how long before its exchanged token's expiry a cached
// backend is re-dialed, mirroring the token exchanger's own skew so the two
// caches expire in step.
const identityBackendSkew = 30 * time.Second

// perIdentityBackend serves each authenticated principal a backend that speaks
// to the user's own backend account: it reads the principal + raw token from the
// request context, exchanges the token (RFC 8693) for a backend-audience token
// preserving the user's sub, and dials with it (MB720-41 foundation; MB720-42
// JMAP, MB720-43 IMAP/SMTP). dial receives the principal so a protocol whose
// auth needs the username (OAUTHBEARER authzid) can use issSub.Sub.
//
// There are three modes, one per constructor:
//
//   - Caching (newPerIdentityBackend): one backend is cached per principal until
//     its exchanged token nears expiry, so a burst of requests does not re-dial.
//     The cache hands the SAME backend value to concurrent and successive requests
//     — safe ONLY because the JMAP/DAV backends' Close is a no-op (stateless HTTP),
//     so the per-request `defer b.Close()` in the handlers does not tear down the
//     shared client.
//   - No-cache (newPerIdentityDialer): dials a fresh backend per request, which
//     the handler then closes. The fallback for a backend with a real Close.
//   - Pooled (newPerIdentityPool): a per-principal connection pool for a backend
//     with a real Close (IMAP/SMTP, whose Close issues LOGOUT/QUIT and drops the
//     TCP connection). get leases a parked connection and the lease's Close
//     returns it to the pool rather than dropping the socket, so the handlers keep
//     their unchanged `defer b.Close()` while the per-request connect + TLS + SASL
//     handshake is amortized. See connPool.
//
// In every mode the token itself is cached by the exchanger, so a re-dial does not
// re-hit the authorization server.
type perIdentityBackend[B io.Closer] struct {
	exchanger tokenexchange.Exchanger
	audience  string
	dial      func(id subjectid.IssSubID, token string) (B, error)

	// mailbox and rawToken read the authenticated identity and bearer token from
	// the request context. They default to auth.Mailbox / auth.RawToken and are
	// fields only so tests can inject a principal without the full middleware.
	mailbox  func(context.Context) (subjectid.SubjectIdentifier, bool)
	rawToken func(context.Context) (string, bool)

	// noCache dials a fresh backend on every get and never caches — required for
	// backends with a real Close when pooling is not in use (see the type doc).
	noCache bool
	skew    time.Duration
	now     func() time.Time

	// pool, when non-nil, selects connection-pooling mode (MB720-53): get checks
	// out a parked connection for the principal (or dials one) and hands back a
	// lease whose Close returns it to the pool rather than dropping the socket.
	// Used for backends with a real Close (IMAP/SMTP) in place of noCache's
	// dial-per-request. Mutually exclusive with the cache.
	pool *connPool[B]

	mu    sync.Mutex
	cache map[subjectid.IssSubID]identityEntry[B]
}

// identityEntry is a cached backend and the expiry of the exchanged token it
// authenticates with.
type identityEntry[B io.Closer] struct {
	backend   B
	expiresAt time.Time
}

// newPerIdentityBackend builds a CACHING per-identity provider (one backend per
// principal until its token nears expiry). Use it only for backends whose Close
// is a no-op (JMAP); see the type doc.
func newPerIdentityBackend[B io.Closer](exchanger tokenexchange.Exchanger, audience string, dial func(id subjectid.IssSubID, token string) (B, error)) *perIdentityBackend[B] {
	return &perIdentityBackend[B]{
		exchanger: exchanger,
		audience:  audience,
		dial:      dial,
		mailbox:   auth.Mailbox,
		rawToken:  auth.RawToken,
		skew:      identityBackendSkew,
		now:       time.Now,
		cache:     make(map[subjectid.IssSubID]identityEntry[B]),
	}
}

// newPerIdentityDialer builds a NON-CACHING per-identity provider: it dials a
// fresh backend on every request, which the handler then closes. Use it for
// backends with a real Close (IMAP/SMTP); see the type doc.
func newPerIdentityDialer[B io.Closer](exchanger tokenexchange.Exchanger, audience string, dial func(id subjectid.IssSubID, token string) (B, error)) *perIdentityBackend[B] {
	return &perIdentityBackend[B]{
		exchanger: exchanger,
		audience:  audience,
		dial:      dial,
		mailbox:   auth.Mailbox,
		rawToken:  auth.RawToken,
		noCache:   true,
		now:       time.Now,
	}
}

// poolOptions configures connection-pooling mode for newPerIdentityPool. wrap is
// required (it builds the lease whose Close returns the connection to the pool);
// the rest fall back to sensible defaults when zero.
type poolOptions[B io.Closer] struct {
	// wrap builds the per-checkout lease for backend type B — see connPool.wrap.
	wrap func(conn B, release func()) B
	// healthCheck probes a parked connection on checkout (NOOP); nil skips it.
	healthCheck func(context.Context, B) error
	// maxIdle bounds parked connections per principal (0 → defaultMaxIdlePerPrincipal).
	maxIdle int
	// idleTimeout evicts a connection parked longer than this (0 → defaultIdleTimeout).
	idleTimeout time.Duration
}

// newPerIdentityPool builds a POOLING per-identity provider for a backend with a
// real Close (IMAP/SMTP): it hands out a parked connection per principal and the
// returned lease's Close returns it to the pool, amortizing the per-request
// connect + TLS + SASL handshake. The caller starts the pool's reaper with
// startPoolReaper bound to the server's shutdown context. See the type doc.
func newPerIdentityPool[B io.Closer](exchanger tokenexchange.Exchanger, audience string, dial func(id subjectid.IssSubID, token string) (B, error), opts poolOptions[B]) *perIdentityBackend[B] {
	maxIdle := opts.maxIdle
	if maxIdle == 0 {
		maxIdle = defaultMaxIdlePerPrincipal
	}
	idleTimeout := opts.idleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultIdleTimeout
	}
	return &perIdentityBackend[B]{
		exchanger: exchanger,
		audience:  audience,
		dial:      dial,
		mailbox:   auth.Mailbox,
		rawToken:  auth.RawToken,
		noCache:   true, // pooling supersedes the cache; the cache map stays unused
		skew:      identityBackendSkew,
		now:       time.Now,
		pool: &connPool[B]{
			wrap:        opts.wrap,
			healthCheck: opts.healthCheck,
			maxIdle:     maxIdle,
			idleTimeout: idleTimeout,
			skew:        identityBackendSkew,
			now:         time.Now,
			idle:        make(map[subjectid.IssSubID][]idleConn[B]),
		},
	}
}

// get resolves the request's backend: a cached one for the principal when its
// token is still fresh, else a freshly exchanged-and-dialed one. In pooling mode
// it leases a parked connection (or dials one), reusing it across requests.
func (p *perIdentityBackend[B]) get(ctx context.Context) (B, error) {
	var zero B
	id, ok := p.mailbox(ctx)
	if !ok {
		return zero, fmt.Errorf("per-identity backend: request is not authenticated")
	}
	// The mailbox identity is the cache key; it must be an RFC 9493 (iss, sub)
	// pair, which is what the auth middleware always stores.
	issSub, ok := id.(subjectid.IssSubID)
	if !ok {
		return zero, fmt.Errorf("per-identity backend: identity format %q is not iss_sub", id.Format())
	}
	raw, ok := p.rawToken(ctx)
	if !ok {
		return zero, fmt.Errorf("per-identity backend: no bearer token on the request")
	}
	now := p.now()

	// Pooling mode: lease a parked connection for the principal, dialing a fresh
	// one (exchange + connect) only on a pool miss. The exchanger still caches the
	// token, so a miss does not re-hit the authorization server.
	if p.pool != nil {
		return p.pool.get(ctx, issSub, func() (B, time.Time, error) {
			var zero B
			tok, err := p.exchanger.Exchange(ctx, raw, p.audience)
			if err != nil {
				return zero, time.Time{}, fmt.Errorf("per-identity backend: token exchange: %w", err)
			}
			backend, err := p.dial(issSub, tok.AccessToken)
			if err != nil {
				return zero, time.Time{}, fmt.Errorf("per-identity backend: dial: %w", err)
			}
			return backend, tok.ExpiresAt, nil
		})
	}

	if !p.noCache {
		p.mu.Lock()
		if e, ok := p.cache[issSub]; ok && now.Before(e.expiresAt.Add(-p.skew)) {
			p.mu.Unlock()
			return e.backend, nil
		}
		p.mu.Unlock()
	}

	// Miss (or no-cache): exchange (the exchanger caches the token) and dial,
	// outside the lock so a slow backend does not block cache reads for others.
	tok, err := p.exchanger.Exchange(ctx, raw, p.audience)
	if err != nil {
		return zero, fmt.Errorf("per-identity backend: token exchange: %w", err)
	}
	backend, err := p.dial(issSub, tok.AccessToken)
	if err != nil {
		return zero, fmt.Errorf("per-identity backend: dial: %w", err)
	}
	// Cache only when caching is enabled and the token has a known expiry; a
	// lifetime-less token cannot be invalidated on time, so re-dial each request.
	if !p.noCache && !tok.ExpiresAt.IsZero() {
		p.mu.Lock()
		p.cache[issSub] = identityEntry[B]{backend: backend, expiresAt: tok.ExpiresAt}
		p.mu.Unlock()
	}
	return backend, nil
}

// mailIdentityProvider adapts a perIdentityBackend to server.MailProvider for
// either JMAP or IMAP; proto labels which, for startup logging.
type mailIdentityProvider struct {
	p     *perIdentityBackend[mail.Backend]
	proto string
}

func (m mailIdentityProvider) Mail(ctx context.Context) (mail.Backend, error) {
	return m.p.get(ctx)
}

// contactsIdentityProvider adapts a perIdentityBackend to server.ContactsProvider
// for either JMAP or CardDAV; proto labels which, for startup logging.
type contactsIdentityProvider struct {
	p     *perIdentityBackend[contacts.Backend]
	proto string
}

func (c contactsIdentityProvider) Contacts(ctx context.Context) (contacts.Backend, error) {
	return c.p.get(ctx)
}

// calendarIdentityProvider adapts a perIdentityBackend to server.CalendarProvider
// (CalDAV today; proto labels it for startup logging).
type calendarIdentityProvider struct {
	p     *perIdentityBackend[calendar.Backend]
	proto string
}

func (c calendarIdentityProvider) Calendar(ctx context.Context) (calendar.Backend, error) {
	return c.p.get(ctx)
}

// schedulingIdentityProvider adapts a perIdentityBackend[smtp.Sender] to
// server.SchedulingProvider: the SMTP sender is per-identity, and the responding
// attendee address (MailboxAddress) is the authenticated principal's subject —
// so the deployment should map -auth-subject-claim to the user's email (MB720-43).
type schedulingIdentityProvider struct {
	p *perIdentityBackend[smtp.Sender]
}

func (s schedulingIdentityProvider) Sender(ctx context.Context) (smtp.Sender, error) {
	return s.p.get(ctx)
}

func (s schedulingIdentityProvider) MailboxAddress(ctx context.Context) (string, error) {
	id, ok := s.p.mailbox(ctx)
	if !ok {
		return "", fmt.Errorf("scheduling: request is not authenticated")
	}
	issSub, ok := id.(subjectid.IssSubID)
	if !ok {
		return "", fmt.Errorf("scheduling: identity format %q is not iss_sub", id.Format())
	}
	return issSub.Sub, nil
}

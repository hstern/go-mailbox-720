package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/auth"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

// identityBackendSkew is how long before its exchanged token's expiry a cached
// backend is re-dialed, mirroring the token exchanger's own skew so the two
// caches expire in step.
const identityBackendSkew = 30 * time.Second

// perIdentityBackend serves each authenticated principal a backend that speaks
// to the user's own backend account: it reads the principal + raw token from the
// request context, exchanges the token (RFC 8693) for a backend-audience token
// preserving the user's sub, and dials with it (MB720-42). One backend is cached
// per principal until its exchanged token nears expiry, so a burst of requests
// from one user does not re-dial (and re-fetch the JMAP Session) each time.
//
// The cache hands the SAME backend value to concurrent and successive requests.
// That is safe only because the JMAP backends' Close is a no-op (JMAP is
// stateless HTTP), so the per-request `defer b.Close()` in the handlers does not
// tear down the shared client. Do NOT reuse this for a backend with a real Close
// (e.g. IMAP, whose Close drops the connection) — that needs connection pooling
// with checkout/return, not a shared instance (MB720-43).
type perIdentityBackend[B io.Closer] struct {
	exchanger tokenexchange.Exchanger
	audience  string
	dial      func(token string) (B, error)

	// mailbox and rawToken read the authenticated identity and bearer token from
	// the request context. They default to auth.Mailbox / auth.RawToken and are
	// fields only so tests can inject a principal without the full middleware.
	mailbox  func(context.Context) (subjectid.SubjectIdentifier, bool)
	rawToken func(context.Context) (string, bool)

	skew time.Duration
	now  func() time.Time

	mu    sync.Mutex
	cache map[subjectid.IssSubID]identityEntry[B]
}

// identityEntry is a cached backend and the expiry of the exchanged token it
// authenticates with.
type identityEntry[B io.Closer] struct {
	backend   B
	expiresAt time.Time
}

// newPerIdentityBackend builds a per-identity provider that exchanges for tokens
// of the given audience and dials with dial. The identity/token readers default
// to the auth middleware's accessors.
func newPerIdentityBackend[B io.Closer](exchanger tokenexchange.Exchanger, audience string, dial func(token string) (B, error)) *perIdentityBackend[B] {
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

// get resolves the request's backend: a cached one for the principal when its
// token is still fresh, else a freshly exchanged-and-dialed one.
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

	p.mu.Lock()
	if e, ok := p.cache[issSub]; ok && now.Before(e.expiresAt.Add(-p.skew)) {
		p.mu.Unlock()
		return e.backend, nil
	}
	p.mu.Unlock()

	// Miss: exchange (the exchanger caches the token) and dial, outside the lock
	// so a slow backend does not block cache reads for other principals.
	tok, err := p.exchanger.Exchange(ctx, raw, p.audience)
	if err != nil {
		return zero, fmt.Errorf("per-identity backend: token exchange: %w", err)
	}
	backend, err := p.dial(tok.AccessToken)
	if err != nil {
		return zero, fmt.Errorf("per-identity backend: dial: %w", err)
	}
	// Cache only when the token has a known expiry; a lifetime-less token cannot
	// be invalidated on time, so re-dial each request (correctness over caching).
	if !tok.ExpiresAt.IsZero() {
		p.mu.Lock()
		p.cache[issSub] = identityEntry[B]{backend: backend, expiresAt: tok.ExpiresAt}
		p.mu.Unlock()
	}
	return backend, nil
}

// jmapMailIdentityProvider adapts a perIdentityBackend to server.MailProvider.
type jmapMailIdentityProvider struct {
	p *perIdentityBackend[mail.Backend]
}

func (j jmapMailIdentityProvider) Mail(ctx context.Context) (mail.Backend, error) {
	return j.p.get(ctx)
}

// jmapContactsIdentityProvider adapts a perIdentityBackend to server.ContactsProvider.
type jmapContactsIdentityProvider struct {
	p *perIdentityBackend[contacts.Backend]
}

func (j jmapContactsIdentityProvider) Contacts(ctx context.Context) (contacts.Backend, error) {
	return j.p.get(ctx)
}

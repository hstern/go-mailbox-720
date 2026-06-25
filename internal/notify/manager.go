package notify

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

// Builder produces the watch and delta-sync adapters for one Graph resource from
// a principal's bearer token, plus the time that token — and therefore the watch
// it authenticates — is valid until. ok is false when the principal's backend
// does not serve this resource (e.g. no JMAP calendar is configured), so the
// manager skips it. Building typically exchanges the token (RFC 8693) and dials
// the JMAP backend, then resolves the principal's primary collection.
type Builder func(ctx context.Context, token string) (watch WatchFunc, sync SyncFunc, expiresAt time.Time, ok bool, err error)

// ResourceBuilder ties a Graph collection path (e.g. "/me/events") to its Builder.
type ResourceBuilder struct {
	Resource string
	Build    Builder
}

// Manager runs at most one watch per principal, (re)started from the bearer token
// the principal presents when it creates or renews a subscription. Each
// per-resource watch loop runs only until that token expires; the principal's
// next renewal re-arms it with a fresh token. This is the renewal-driven,
// best-effort multi-tenant delivery model: push is live while a principal's token
// is fresh and the backend falls back to client polling in the gaps.
//
// Push/watch needs a token that outlives a single request, but the token
// exchanger can only mint a backend token from a live user token (impersonation,
// not a stored credential). The subscription create/renew flow is the one place a
// fresh user token reliably arrives, so the watch lifecycle is bound to it.
type Manager struct {
	base     context.Context
	builders []ResourceBuilder
	store    subscriptions.Store
	client   *http.Client
	now      func() time.Time
	logf     func(format string, args ...any)

	mu      sync.Mutex
	watches map[string]*principalWatch
}

// principalWatch is one principal's running watch: a cancel for its context and a
// WaitGroup over its per-resource loops.
type principalWatch struct {
	cancel context.CancelFunc
	wg     *sync.WaitGroup
}

// NewManager builds a Manager. base is the process-lifetime context; cancelling
// it stops every watch. builders is one entry per watchable Graph resource. store
// and client are passed to the delivery loops (client should be the SSRF-guarded
// delivery client). now and logf default to time.Now and a no-op.
func NewManager(base context.Context, builders []ResourceBuilder, store subscriptions.Store, client *http.Client, now func() time.Time, logf func(string, ...any)) *Manager {
	if now == nil {
		now = time.Now
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		base:     base,
		builders: builders,
		store:    store,
		client:   client,
		now:      now,
		logf:     logf,
		watches:  make(map[string]*principalWatch),
	}
}

// OnSubscribe (re)starts owner's watch using token — the bearer the principal
// just presented on a subscription create or renewal. It scopes the watch to the
// resources owner currently subscribes to and bounds each resource loop by the
// token's expiry, so the loop stops when the token would; the next renewal
// restarts it. A previous watch for owner is torn down in the background. owner
// or token empty (single-tenant / unauthenticated) is a no-op.
func (m *Manager) OnSubscribe(owner, token string) {
	if owner == "" || token == "" {
		return
	}
	resources := m.subscribedResources(owner)
	if len(resources) == 0 {
		return
	}

	pctx, cancel := context.WithCancel(m.base)
	wg := &sync.WaitGroup{}
	started := 0
	for _, rb := range m.builders {
		if !resources[rb.Resource] {
			continue
		}
		watch, sync, expiresAt, ok, err := rb.Build(pctx, token)
		if err != nil {
			m.logf("notify(manager): build %s for principal failed: %v", rb.Resource, err)
			continue
		}
		if !ok {
			continue
		}
		loopCtx, loopCancel := pctx, context.CancelFunc(func() {})
		if !expiresAt.IsZero() {
			// Bound the loop by the token's expiry; a renewal re-arms it.
			loopCtx, loopCancel = context.WithDeadline(pctx, expiresAt)
		}
		started++
		wg.Add(1)
		rb := rb
		go func() {
			defer wg.Done()
			defer loopCancel()
			if err := RunResource(loopCtx, owner, rb.Resource, watch, sync, m.store, m.client, m.now, m.reportFor(rb.Resource)); err != nil {
				m.logf("notify(manager): %s loop for principal stopped: %v", rb.Resource, err)
			}
		}()
	}
	if started == 0 {
		cancel()
		return
	}
	m.swap(owner, &principalWatch{cancel: cancel, wg: wg})
}

// Reap stops the watch of every principal that no longer has an unexpired
// subscription. Call it periodically (the store does not notify on expiry).
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for owner, pw := range m.watches {
		if len(m.subscribedResources(owner)) == 0 {
			delete(m.watches, owner)
			go teardown(pw)
		}
	}
}

// subscribedResources is the set of resources owner has a currently-unexpired
// subscription for.
func (m *Manager) subscribedResources(owner string) map[string]bool {
	now := m.now()
	out := make(map[string]bool)
	for _, s := range m.store.ListByOwner(owner) {
		if s.ExpirationDateTime.After(now) {
			out[s.Resource] = true
		}
	}
	return out
}

// swap installs pw as owner's watch and tears down any previous one in the
// background (so OnSubscribe never blocks on a socket close). A brief overlap of
// old and new loops is harmless: both deliver to the same subscriptions, and the
// notifications are idempotent.
func (m *Manager) swap(owner string, pw *principalWatch) {
	m.mu.Lock()
	old := m.watches[owner]
	m.watches[owner] = pw
	m.mu.Unlock()
	if old != nil {
		go teardown(old)
	}
}

func teardown(pw *principalWatch) {
	pw.cancel()
	pw.wg.Wait()
}

// reportFor returns a delivery-result logger for a resource.
func (m *Manager) reportFor(resource string) func(subscriptions.Result) {
	return func(r subscriptions.Result) {
		if r.Delivered > 0 || len(r.Errors) > 0 {
			m.logf("notify(manager): %s delivered %d/%d (errors=%d)", resource, r.Delivered, r.Matched, len(r.Errors))
		}
	}
}

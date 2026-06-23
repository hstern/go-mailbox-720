package main

import (
	"context"
	"io"
	"sync"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mail/imap"
	"github.com/hstern/go-mailbox-720/internal/smtp"
)

// Pool defaults. A per-principal mail proxy holds at most a handful of in-flight
// requests per user, so a small idle bound suffices; the idle timeout keeps an
// inactive user's connections from lingering well under a typical IMAP server's
// 30-minute autologout (RFC 9051 §5.4).
const (
	defaultMaxIdlePerPrincipal = 4
	defaultIdleTimeout         = 5 * time.Minute
	// poolReapInterval is how often the background reaper sweeps idle-timed-out
	// connections; lazy eviction on checkout handles the rest.
	poolReapInterval = time.Minute
)

// idleConn is a parked connection plus the expiry of the exchanged token it
// authenticates with and the instant it went idle. A connection's auth is only
// as fresh as its token, so expiresAt bounds reuse; idleAt drives idle eviction.
type idleConn[B io.Closer] struct {
	conn      B
	expiresAt time.Time
	idleAt    time.Time
}

// connPool keeps idle backend connections per principal (keyed by IssSubID) and
// hands out a lease — a wrapper whose Close returns the connection to the pool
// instead of tearing it down — so the server handlers keep their unchanged
// `defer b.Close()`. It exists for backends with a real Close (IMAP/SMTP, whose
// Close issues LOGOUT/QUIT and drops the TCP connection); the per-request dial +
// TLS + SASL handshake those otherwise pay is what pooling amortizes.
//
// B must be an interface type: the lease is a *different* concrete type than the
// raw connection (it overrides Close), so it can only stand in for the raw
// connection through an interface. wrap and healthCheck are therefore supplied
// per backend type at construction.
type connPool[B io.Closer] struct {
	// wrap builds the per-checkout lease: a B whose Close calls release (returning
	// the raw connection to the pool) but which otherwise delegates to the raw
	// connection — for IMAP, by embedding it so every optional capability
	// (Writer/DeltaReader/Watcher/QuotaReader/RawReader) stays type-assertable.
	wrap func(conn B, release func()) B
	// healthCheck probes a parked connection on checkout (IMAP/SMTP NOOP) so a
	// server-dropped connection is replaced, not handed out dead. Nil skips it.
	healthCheck func(context.Context, B) error
	// maxIdle bounds parked connections per principal; over-cap check-ins are torn
	// down rather than pooled. <=0 means unbounded.
	maxIdle int
	// idleTimeout evicts a connection parked longer than this. <=0 disables.
	idleTimeout time.Duration
	// skew re-dials a connection this long before its token expires, mirroring the
	// token exchanger's own skew so the two expire in step.
	skew time.Duration
	now  func() time.Time

	mu   sync.Mutex
	idle map[subjectid.IssSubID][]idleConn[B]
}

// get returns a leased connection for the principal: a parked one when a live,
// non-expired connection is available, else a freshly dialed one. dialNew mints a
// new connection and reports the expiry of the token authenticating it (zero =
// unknown lifetime, never poolable). The caller must Close the returned lease,
// which returns the connection to the pool.
func (cp *connPool[B]) get(ctx context.Context, id subjectid.IssSubID, dialNew func() (B, time.Time, error)) (B, error) {
	var zero B
	now := cp.now()
	for {
		c, ok := cp.pop(id)
		if !ok {
			break
		}
		// Discard a connection whose token is near expiry, that has sat idle too
		// long, or that fails its health check — tearing it down outside the lock.
		if cp.expired(c, now) || cp.timedOut(c, now) {
			_ = c.conn.Close()
			continue
		}
		if cp.healthCheck != nil {
			if err := cp.healthCheck(ctx, c.conn); err != nil {
				_ = c.conn.Close()
				continue
			}
		}
		return cp.lease(id, c.conn, c.expiresAt), nil
	}

	conn, expiresAt, err := dialNew()
	if err != nil {
		return zero, err
	}
	return cp.lease(id, conn, expiresAt), nil
}

// pop removes and returns the most-recently-parked connection for the principal,
// or ok=false when none is parked. Most-recently-parked first keeps the warmest
// connection in rotation and lets idle ones age out for the reaper.
func (cp *connPool[B]) pop(id subjectid.IssSubID) (idleConn[B], bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	conns := cp.idle[id]
	if len(conns) == 0 {
		return idleConn[B]{}, false
	}
	last := len(conns) - 1
	c := conns[last]
	conns[last] = idleConn[B]{} // drop the reference so the conn can be GC'd if not reused
	cp.idle[id] = conns[:last]
	if len(cp.idle[id]) == 0 {
		delete(cp.idle, id)
	}
	return c, true
}

// lease wraps conn so its Close returns it to the pool exactly once (a handler
// that Closes twice must not double-return it).
func (cp *connPool[B]) lease(id subjectid.IssSubID, conn B, expiresAt time.Time) B {
	var once sync.Once
	return cp.wrap(conn, func() {
		once.Do(func() { cp.checkin(id, conn, expiresAt) })
	})
}

// checkin returns conn to the pool, or tears it down when it cannot be reused:
// an unknown-lifetime or near-expiry token, or the principal's idle bound is full.
func (cp *connPool[B]) checkin(id subjectid.IssSubID, conn B, expiresAt time.Time) {
	now := cp.now()
	if expiresAt.IsZero() || !now.Before(expiresAt.Add(-cp.skew)) {
		_ = conn.Close()
		return
	}
	cp.mu.Lock()
	if cp.maxIdle > 0 && len(cp.idle[id]) >= cp.maxIdle {
		cp.mu.Unlock()
		_ = conn.Close()
		return
	}
	cp.idle[id] = append(cp.idle[id], idleConn[B]{conn: conn, expiresAt: expiresAt, idleAt: now})
	cp.mu.Unlock()
}

// reap tears down every parked connection that has aged past the idle timeout or
// whose token is near expiry. It is safe to call concurrently with get/checkin
// and is driven by a background ticker so an inactive principal's connections do
// not linger until the process exits.
func (cp *connPool[B]) reap() {
	now := cp.now()
	var dead []B
	cp.mu.Lock()
	for id, conns := range cp.idle {
		kept := conns[:0]
		for _, c := range conns {
			if cp.expired(c, now) || cp.timedOut(c, now) {
				dead = append(dead, c.conn)
				continue
			}
			kept = append(kept, c)
		}
		// Clear the now-stale tail of the backing array so it does not retain
		// references to the reaped (and soon-closed) connections.
		for i := len(kept); i < len(conns); i++ {
			conns[i] = idleConn[B]{}
		}
		if len(kept) == 0 {
			delete(cp.idle, id)
		} else {
			cp.idle[id] = kept
		}
	}
	cp.mu.Unlock()
	for _, c := range dead {
		_ = c.Close()
	}
}

// closeAll tears down every parked connection with a real Close (LOGOUT/QUIT) and
// empties the pool. It is called during graceful shutdown so the upstream server
// reclaims the principals' sessions immediately rather than waiting for its own
// idle timeout to reap the abandoned sockets. In-flight leases are unaffected:
// their Close will find the pool empty and tear the connection down.
func (cp *connPool[B]) closeAll() {
	cp.mu.Lock()
	parked := cp.idle
	cp.idle = make(map[subjectid.IssSubID][]idleConn[B])
	cp.mu.Unlock()
	for _, conns := range parked {
		for _, c := range conns {
			_ = c.conn.Close()
		}
	}
}

// expired reports whether the connection's token is within skew of expiry.
func (cp *connPool[B]) expired(c idleConn[B], now time.Time) bool {
	return c.expiresAt.IsZero() || !now.Before(c.expiresAt.Add(-cp.skew))
}

// timedOut reports whether the connection has sat idle past the idle timeout.
func (cp *connPool[B]) timedOut(c idleConn[B], now time.Time) bool {
	return cp.idleTimeout > 0 && now.Sub(c.idleAt) > cp.idleTimeout
}

// pooledIMAP is the IMAP connection-pool lease. It embeds the concrete
// *imap.Client so every optional capability the server handlers type-assert for
// (mail.Writer, mail.DeltaReader, mail.Watcher, mail.QuotaReader, mail.RawReader,
// mail.FilterReader, mail.FilterWriter) stays promoted and assertable; only Close
// is overridden, to return the connection to the pool instead of issuing LOGOUT.
//
// A returned connection may be left in a SELECTED state — notably the notifier's
// mail.Watcher connection, which SELECTs INBOX to run IDLE. That is safe to hand
// to the next request unchanged: every adapter operation (ListMessages,
// GetMessage, the writes, delta, raw) issues its own SELECT first, so it never
// assumes a freshly-logged-in connection, and the NOOP health check confirms the
// connection is still alive regardless of its selected mailbox.
type pooledIMAP struct {
	*imap.Client
	release func()
}

func (p *pooledIMAP) Close() error { p.release(); return nil }

// The lease must satisfy mail.Backend AND every optional capability the IMAP
// client offers, so the server handlers' type assertions keep working through the
// pool. Embedding *imap.Client promotes them all; these assertions fail the build
// if a future change (e.g. dropping the embed, or the client gaining a capability
// the wrapper shadows) silently loses one.
var (
	_ mail.Backend      = (*pooledIMAP)(nil)
	_ mail.Writer       = (*pooledIMAP)(nil)
	_ mail.DeltaReader  = (*pooledIMAP)(nil)
	_ mail.Watcher      = (*pooledIMAP)(nil)
	_ mail.QuotaReader  = (*pooledIMAP)(nil)
	_ mail.RawReader    = (*pooledIMAP)(nil)
	_ mail.FilterReader = (*pooledIMAP)(nil)
	_ mail.FilterWriter = (*pooledIMAP)(nil)
)

// imapPoolOptions builds the pool hooks for IMAP mail backends: a lease that
// preserves the client's optional interfaces and a NOOP health check on checkout.
func imapPoolOptions() poolOptions[mail.Backend] {
	return poolOptions[mail.Backend]{
		wrap: func(conn mail.Backend, release func()) mail.Backend {
			return &pooledIMAP{Client: conn.(*imap.Client), release: release}
		},
		healthCheck: func(ctx context.Context, conn mail.Backend) error {
			return conn.(*imap.Client).Ping(ctx)
		},
	}
}

// pooledSMTP is the SMTP connection-pool lease. smtp.Sender exposes no optional
// capabilities, so it embeds the interface and overrides only Close.
type pooledSMTP struct {
	smtp.Sender
	release func()
}

func (p *pooledSMTP) Close() error { p.release(); return nil }

var _ smtp.Sender = (*pooledSMTP)(nil)

// smtpPoolOptions builds the pool hooks for SMTP senders: a lease over the Sender
// interface and a NOOP health check on checkout.
func smtpPoolOptions() poolOptions[smtp.Sender] {
	return poolOptions[smtp.Sender]{
		wrap: func(conn smtp.Sender, release func()) smtp.Sender {
			return &pooledSMTP{Sender: conn, release: release}
		},
		healthCheck: func(ctx context.Context, conn smtp.Sender) error {
			if c, ok := conn.(*smtp.Client); ok {
				return c.Ping(ctx)
			}
			return nil
		},
	}
}

// connPoolHook bundles a pool's background reaper and its shutdown drain so the
// server lifecycle can manage both without knowing the pool's element type
// (connPool[mail.Backend] and connPool[smtp.Sender] are distinct types).
type connPoolHook struct {
	reap     func()
	closeAll func()
}

// startPoolReaper runs reap on a ticker until ctx is cancelled, mirroring the
// notifier's ctx-bound goroutine so it shuts down with the server.
func startPoolReaper(ctx context.Context, interval time.Duration, reap func()) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reap()
			}
		}
	}()
}

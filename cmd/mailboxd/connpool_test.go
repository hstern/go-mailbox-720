package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

// poolConn is the interface the connection pool is instantiated over in these
// tests, mirroring how production instantiates it over mail.Backend / smtp.Sender
// (interfaces, so a Close-overriding wrapper of a different concrete type can
// substitute for the raw connection).
type poolConn interface {
	io.Closer
}

// poolFake is a raw pooled connection: it records Close calls and can be told to
// fail its health check.
type poolFake struct {
	id        int
	mu        sync.Mutex
	closes    int
	healthErr error
}

func (f *poolFake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *poolFake) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// leasedFake is the per-checkout wrapper whose Close returns the raw connection
// to the pool instead of tearing it down — the test analogue of pooledIMAP.
type leasedFake struct {
	poolConn
	release func()
}

func (l *leasedFake) Close() error { l.release(); return nil }

// poolTestWrap and poolTestHealth are the type-specific hooks the pool needs.
func poolTestWrap(conn poolConn, release func()) poolConn {
	return &leasedFake{poolConn: conn, release: release}
}

func poolTestHealth(_ context.Context, conn poolConn) error {
	return conn.(*poolFake).healthErr
}

// newTestPool builds a connPool over poolConn with a controllable clock.
func newTestPool(now func() time.Time, maxIdle int, idleTimeout time.Duration) *connPool[poolConn] {
	return &connPool[poolConn]{
		wrap:        poolTestWrap,
		healthCheck: poolTestHealth,
		maxIdle:     maxIdle,
		idleTimeout: idleTimeout,
		skew:        time.Minute,
		now:         now,
		idle:        make(map[subjectid.IssSubID][]idleConn[poolConn]),
	}
}

// dialFactory hands out a fresh poolFake per call with the given token expiry.
type dialFactory struct {
	mu        sync.Mutex
	calls     int
	expiresAt time.Time
	err       error
	conns     []*poolFake
}

func (d *dialFactory) dial() (poolConn, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return nil, time.Time{}, d.err
	}
	c := &poolFake{id: d.calls}
	d.conns = append(d.conns, c)
	return c, d.expiresAt, nil
}

func (d *dialFactory) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func poolTestID() subjectid.IssSubID { return subjectid.IssSubID{Iss: testIss, Sub: "alice"} }

func TestConnPoolReusesIdleConn(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	now := base
	cp := newTestPool(func() time.Time { return now }, 4, 0)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	first, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if err := first.Close(); err != nil { // returns the conn to the pool
		t.Fatalf("close: %v", err)
	}
	second, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if df.callCount() != 1 {
		t.Errorf("dialed %d times, want 1 (reused the idle connection)", df.callCount())
	}
	// The raw connection underneath both leases must be the same object.
	if rawOf(first) != rawOf(second) {
		t.Error("second get handed out a different underlying connection")
	}
}

func TestConnPoolGrowsUnderConcurrency(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	// Two simultaneous leases (neither released) must each get their own conn.
	a, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	b, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (a second in-flight request grows the pool)", df.callCount())
	}
	if rawOf(a) == rawOf(b) {
		t.Error("two concurrent leases shared one connection")
	}
	_ = a.Close()
	_ = b.Close()
}

func TestConnPoolHealthCheckDiscardsDead(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	first, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	raw := rawOf(first)
	raw.healthErr = errors.New("connection reset") // server dropped it while idle
	_ = first.Close()                              // returns the (now-dead) conn to the pool

	second, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (dead idle conn replaced)", df.callCount())
	}
	if raw.closeCount() == 0 {
		t.Error("dead connection was not torn down on checkout")
	}
	if rawOf(second) == raw {
		t.Error("handed out the dead connection")
	}
}

func TestConnPoolBoundedMaxIdle(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 1, 0) // hold at most 1 idle
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	a, _ := cp.get(context.Background(), poolTestID(), df.dial)
	b, _ := cp.get(context.Background(), poolTestID(), df.dial)
	rawB := rawOf(b)
	_ = a.Close() // pooled (idle now has 1)
	_ = b.Close() // pool full → torn down, not pooled
	if rawB.closeCount() == 0 {
		t.Error("over-cap connection should be torn down on check-in, not pooled")
	}
}

func TestConnPoolEvictsNearExpiry(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	now := base
	cp := newTestPool(func() time.Time { return now }, 4, 0) // skew = 1m
	df := &dialFactory{expiresAt: base.Add(2 * time.Minute)}

	first, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	raw := rawOf(first)
	_ = first.Close() // still > skew from expiry → pooled

	// Advance to within skew of the token's expiry: the pooled conn's auth is
	// about to die, so checkout must discard it and dial afresh.
	now = base.Add(90 * time.Second) // 30s before the 2m expiry, inside the 1m skew
	second, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (near-expiry conn evicted)", df.callCount())
	}
	if raw.closeCount() == 0 {
		t.Error("near-expiry conn was not torn down")
	}
	_ = second.Close()
}

func TestConnPoolUncacheableNotPooled(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	df := &dialFactory{} // zero expiresAt: lifetime unknown, never poolable

	first, err := cp.get(context.Background(), poolTestID(), df.dial)
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	raw := rawOf(first)
	_ = first.Close()
	if raw.closeCount() == 0 {
		t.Error("a connection with an unknown-lifetime token must not be pooled")
	}
	if _, err := cp.get(context.Background(), poolTestID(), df.dial); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (uncacheable conn re-dialed)", df.callCount())
	}
}

func TestConnPoolReapDropsIdleTimedOut(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	now := base
	cp := newTestPool(func() time.Time { return now }, 4, 5*time.Minute)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	first, _ := cp.get(context.Background(), poolTestID(), df.dial)
	raw := rawOf(first)
	_ = first.Close() // idle since base

	now = base.Add(6 * time.Minute) // past the 5m idle timeout
	cp.reap()
	if raw.closeCount() == 0 {
		t.Error("reap did not close the idle-timed-out connection")
	}
	// Pool should be empty now: next get dials.
	if _, err := cp.get(context.Background(), poolTestID(), df.dial); err != nil {
		t.Fatalf("get after reap: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (reaped conn gone)", df.callCount())
	}
}

func TestConnPoolDialError(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	sentinel := errors.New("dial boom")
	df := &dialFactory{err: sentinel}

	if _, err := cp.get(context.Background(), poolTestID(), df.dial); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestConnPoolDoubleCloseSafe(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	lease, _ := cp.get(context.Background(), poolTestID(), df.dial)
	_ = lease.Close()
	_ = lease.Close() // second Close must not double-return the conn to the pool

	// Exactly one idle conn should be present: a single get reuses it, no dial.
	if _, err := cp.get(context.Background(), poolTestID(), df.dial); err != nil {
		t.Fatalf("get: %v", err)
	}
	if df.callCount() != 1 {
		t.Errorf("dialed %d times, want 1 (double Close must not corrupt the pool)", df.callCount())
	}
}

// TestConnPoolThroughPerIdentity exercises the pool via newPerIdentityPool: the
// first request exchanges a token and dials; after the lease is returned, the
// next request reuses the idle connection without exchanging or dialing again.
func TestConnPoolThroughPerIdentity(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	now := base
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: base.Add(time.Hour)}}
	df := &dialFactory{expiresAt: base.Add(time.Hour)}
	dial := func(_ subjectid.IssSubID, _ string) (poolConn, error) {
		c, _, err := df.dial()
		return c, err
	}
	p := newPerIdentityPool[poolConn](ex, "backend-aud", dial, poolOptions[poolConn]{
		wrap:        poolTestWrap,
		healthCheck: poolTestHealth,
		maxIdle:     4,
	})
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return testID("alice"), true }
	p.rawToken = func(context.Context) (string, bool) { return testRaw, true }
	p.now = func() time.Time { return now }
	p.pool.now = func() time.Time { return now }

	first, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if ex.callCount() != 1 || df.callCount() != 1 {
		t.Fatalf("first get: exchange=%d dial=%d, want 1/1", ex.callCount(), df.callCount())
	}
	_ = first.Close() // return to pool

	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if ex.callCount() != 1 {
		t.Errorf("exchange called %d times, want 1 (reuse skips the exchange)", ex.callCount())
	}
	if df.callCount() != 1 {
		t.Errorf("dialed %d times, want 1 (reuse skips the dial)", df.callCount())
	}
}

func TestConnPoolCloseAll(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cp := newTestPool(func() time.Time { return base }, 4, 0)
	df := &dialFactory{expiresAt: base.Add(time.Hour)}

	first, _ := cp.get(context.Background(), poolTestID(), df.dial)
	raw := rawOf(first)
	_ = first.Close() // park it

	cp.closeAll()
	if raw.closeCount() == 0 {
		t.Error("closeAll did not tear down the parked connection")
	}
	// Pool is drained: the next get must dial afresh.
	if _, err := cp.get(context.Background(), poolTestID(), df.dial); err != nil {
		t.Fatalf("get after drain: %v", err)
	}
	if df.callCount() != 2 {
		t.Errorf("dialed %d times, want 2 (drained pool re-dials)", df.callCount())
	}
}

// rawOf unwraps a leasedFake to the raw poolFake underneath it.
func rawOf(c poolConn) *poolFake {
	if l, ok := c.(*leasedFake); ok {
		return l.poolConn.(*poolFake)
	}
	return c.(*poolFake)
}

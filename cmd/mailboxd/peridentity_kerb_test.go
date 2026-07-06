package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	subjectid "github.com/hstern/go-subjectid"
)

// fakeKerbExchanger is the Kerberos twin of fakeExchanger: it records its calls
// (and the requested output type / target) and returns a canned credential or an
// error.
type fakeKerbExchanger struct {
	cred *kerb.Credential
	err  error

	mu         sync.Mutex
	calls      int
	lastOutput kerb.OutputType
	lastTarget kerb.ServicePrincipal
}

func (e *fakeKerbExchanger) Exchange(_ context.Context, _ string, target kerb.ServicePrincipal, output kerb.OutputType) (*kerb.Credential, error) {
	e.mu.Lock()
	e.calls++
	e.lastOutput = output
	e.lastTarget = target
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return e.cred, nil
}

func (e *fakeKerbExchanger) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

var testSPN = kerb.ServicePrincipal{Service: "imap", Host: "mail.example.com", Realm: "EXAMPLE.COM"}

// newKerbPool wires a perIdentityKerbBackend over the shared poolConn test
// helpers, fixed to principal "alice" at now.
func newKerbPool(t *testing.T, ex *fakeKerbExchanger, df *dialFactory, now func() time.Time) *perIdentityKerbBackend[poolConn] {
	t.Helper()
	dial := func(_ subjectid.IssSubID, _ *kerb.Credential) (poolConn, error) {
		c, _, err := df.dial()
		return c, err
	}
	p := newPerIdentityKerbPool[poolConn](ex, testSPN, dial, poolOptions[poolConn]{
		wrap:        poolTestWrap,
		healthCheck: poolTestHealth,
		maxIdle:     4,
	})
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return testID("alice"), true }
	p.rawToken = func(context.Context) (string, bool) { return testRaw, true }
	p.now = now
	p.pool.now = now
	return p
}

// TestKerbPoolExchangesAndReuses: the first request exchanges a ccache and dials;
// after the lease returns, the next request reuses the idle connection without
// exchanging or dialing again. Also asserts the exchange requests OutputCCache.
func TestKerbPoolExchangesAndReuses(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	now := base
	cred := kerb.NewCredential("alice", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	ex := &fakeKerbExchanger{cred: cred}
	df := &dialFactory{expiresAt: base.Add(time.Hour)}
	p := newKerbPool(t, ex, df, func() time.Time { return now })

	first, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if ex.callCount() != 1 || df.callCount() != 1 {
		t.Fatalf("first get: exchange=%d dial=%d, want 1/1", ex.callCount(), df.callCount())
	}
	if ex.lastOutput != kerb.OutputCCache {
		t.Errorf("exchange output = %v, want OutputCCache", ex.lastOutput)
	}
	if ex.lastTarget != testSPN {
		t.Errorf("exchange target = %v, want %v", ex.lastTarget, testSPN)
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

// TestKerbPoolReDialsAfterExpiry: once the ticket's expiry passes, the parked
// connection is evicted and the next request exchanges and dials afresh.
func TestKerbPoolReDialsAfterExpiry(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	now := base
	cred := kerb.NewCredential("alice", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	ex := &fakeKerbExchanger{cred: cred}
	df := &dialFactory{expiresAt: base.Add(time.Hour)}
	p := newKerbPool(t, ex, df, func() time.Time { return now })

	first, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	_ = first.Close() // park it

	now = base.Add(time.Hour) // at/after expiry (minus skew)
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if ex.callCount() != 2 || df.callCount() != 2 {
		t.Errorf("after expiry: exchange=%d dial=%d, want 2/2 (re-dial)", ex.callCount(), df.callCount())
	}
}

// TestKerbPoolDistinctPrincipals: two principals get independently exchanged
// credentials and dialed connections.
func TestKerbPoolDistinctPrincipals(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	cred := kerb.NewCredential("x", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	ex := &fakeKerbExchanger{cred: cred}
	df := &dialFactory{expiresAt: base.Add(time.Hour)}
	p := newKerbPool(t, ex, df, func() time.Time { return base })

	who := "alice"
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return testID(who), true }

	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("get alice: %v", err)
	}
	who = "bob"
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if ex.callCount() != 2 || df.callCount() != 2 {
		t.Errorf("distinct principals: exchange=%d dial=%d, want 2/2", ex.callCount(), df.callCount())
	}
}

func TestKerbPoolExchangeError(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	sentinel := errors.New("boom")
	ex := &fakeKerbExchanger{err: sentinel}
	df := &dialFactory{expiresAt: base.Add(time.Hour)}
	p := newKerbPool(t, ex, df, func() time.Time { return base })

	if _, err := p.get(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("get: error = %v, want it to wrap the exchange error", err)
	}
	if df.callCount() != 0 {
		t.Errorf("dialed %d times after an exchange error, want 0", df.callCount())
	}
}

func TestKerbPoolDialError(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	cred := kerb.NewCredential("alice", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	ex := &fakeKerbExchanger{cred: cred}
	sentinel := errors.New("dial boom")
	df := &dialFactory{err: sentinel}
	p := newKerbPool(t, ex, df, func() time.Time { return base })

	if _, err := p.get(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("get: error = %v, want it to wrap the dial error", err)
	}
}

func TestKerbPoolNoIdentity(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	cred := kerb.NewCredential("alice", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	p := newKerbPool(t, &fakeKerbExchanger{cred: cred}, &dialFactory{expiresAt: base.Add(time.Hour)}, func() time.Time { return base })
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return nil, false }

	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("get without an identity: want error, got nil")
	}
}

func TestKerbPoolNoToken(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	cred := kerb.NewCredential("alice", testSPN, base.Add(time.Hour), []byte("ccache"), nil)
	p := newKerbPool(t, &fakeKerbExchanger{cred: cred}, &dialFactory{expiresAt: base.Add(time.Hour)}, func() time.Time { return base })
	p.rawToken = func(context.Context) (string, bool) { return "", false }

	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("get without a token: want error, got nil")
	}
}

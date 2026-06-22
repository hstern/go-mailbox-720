package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

// fakeBackend is a minimal io.Closer standing in for a JMAP backend.
type fakeBackend struct{ id int }

func (f *fakeBackend) Close() error { return nil }

// fakeExchanger records its calls and returns a canned token or error.
type fakeExchanger struct {
	mu      sync.Mutex
	calls   int
	subject []string
	aud     []string
	tok     tokenexchange.Token
	err     error
}

func (e *fakeExchanger) Exchange(_ context.Context, subjectToken, audience string) (tokenexchange.Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.subject = append(e.subject, subjectToken)
	e.aud = append(e.aud, audience)
	if e.err != nil {
		return tokenexchange.Token{}, e.err
	}
	return e.tok, nil
}

func (e *fakeExchanger) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// dialRecorder records the tokens dial was called with and hands out a fresh
// fakeBackend each time (or a canned error).
type dialRecorder struct {
	mu     sync.Mutex
	calls  int
	tokens []string
	err    error
}

func (d *dialRecorder) dial(token string) (*fakeBackend, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.tokens = append(d.tokens, token)
	if d.err != nil {
		return nil, d.err
	}
	return &fakeBackend{id: d.calls}, nil
}

func (d *dialRecorder) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const (
	testIss = "https://issuer.example/"
	testRaw = "user-bearer-token"
)

func testID(sub string) subjectid.IssSubID { return subjectid.IssSubID{Iss: testIss, Sub: sub} }

// configure wires a provider with fixed identity + token readers and a clock.
func configure(p *perIdentityBackend[*fakeBackend], id subjectid.SubjectIdentifier, idOK bool, raw string, rawOK bool, now func() time.Time) {
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return id, idOK }
	p.rawToken = func(context.Context) (string, bool) { return raw, rawOK }
	if now != nil {
		p.now = now
	}
}

func TestPerIdentityExchangesAndDials(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: time.Unix(1<<40, 0)}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, nil)

	b, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if b == nil {
		t.Fatal("nil backend")
	}
	if len(ex.subject) != 1 || ex.subject[0] != testRaw {
		t.Errorf("exchange subject = %v, want [%s]", ex.subject, testRaw)
	}
	if len(ex.aud) != 1 || ex.aud[0] != "backend-aud" {
		t.Errorf("exchange audience = %v, want [backend-aud]", ex.aud)
	}
	if len(dr.tokens) != 1 || dr.tokens[0] != "exchanged" {
		t.Errorf("dial token = %v, want [exchanged]", dr.tokens)
	}
}

func TestPerIdentityCachesPerPrincipal(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: base.Add(time.Hour)}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, func() time.Time { return now })

	first, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	now = base.Add(30 * time.Minute) // well within the hour
	second, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if first != second {
		t.Error("cache miss: got a different backend within the token lifetime")
	}
	if dr.callCount() != 1 {
		t.Errorf("dial called %d times, want 1 (cache hit)", dr.callCount())
	}
	if ex.callCount() != 1 {
		t.Errorf("exchange called %d times, want 1 (cache hit)", ex.callCount())
	}
}

func TestPerIdentityReDialsAfterExpiry(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: base.Add(time.Hour)}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	p.skew = time.Minute
	configure(p, testID("alice"), true, testRaw, true, func() time.Time { return now })

	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("first get: %v", err)
	}
	// Past expiry - skew (60min - 1min = 59min): move to 59min1s → re-dial.
	now = base.Add(59*time.Minute + time.Second)
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if dr.callCount() != 2 {
		t.Errorf("dial called %d times, want 2 (re-dial after expiry)", dr.callCount())
	}
}

func TestPerIdentityDistinctPrincipals(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: base.Add(time.Hour)}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	p.now = func() time.Time { return base }

	// alice
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return testID("alice"), true }
	p.rawToken = func(context.Context) (string, bool) { return "alice-tok", true }
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("alice get: %v", err)
	}
	// bob
	p.mailbox = func(context.Context) (subjectid.SubjectIdentifier, bool) { return testID("bob"), true }
	p.rawToken = func(context.Context) (string, bool) { return "bob-tok", true }
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if dr.callCount() != 2 {
		t.Errorf("dial called %d times, want 2 (distinct principals)", dr.callCount())
	}
}

func TestPerIdentityNoIdentity(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged"}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, nil, false, testRaw, true, nil)

	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("want error when the request is unauthenticated")
	}
	if dr.callCount() != 0 || ex.callCount() != 0 {
		t.Error("dial/exchange must not run without an identity")
	}
}

func TestPerIdentityNoToken(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged"}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, "", false, nil)

	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("want error when no bearer token is present")
	}
	if dr.callCount() != 0 || ex.callCount() != 0 {
		t.Error("dial/exchange must not run without a token")
	}
}

func TestPerIdentityNonIssSubIdentity(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged"}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	// An email-format identity is well-formed but not the iss_sub pair the cache
	// keys on, so the provider rejects it rather than mis-key the cache.
	configure(p, subjectid.EmailID{Email: "alice@example.com"}, true, testRaw, true, nil)

	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("want error for a non-iss_sub identity")
	}
	if dr.callCount() != 0 {
		t.Error("dial must not run for an unkeyable identity")
	}
}

func TestPerIdentityExchangeError(t *testing.T) {
	sentinel := errors.New("exchange boom")
	ex := &fakeExchanger{err: sentinel}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, nil)

	_, err := p.get(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
	if dr.callCount() != 0 {
		t.Error("dial must not run when the exchange fails")
	}
}

func TestPerIdentityDialError(t *testing.T) {
	sentinel := errors.New("dial boom")
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: time.Unix(1<<40, 0)}}
	dr := &dialRecorder{err: sentinel}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, nil)

	if _, err := p.get(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestPerIdentityNoLifetimeNotCached(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged"}} // zero ExpiresAt
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, nil)

	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("first get: %v", err)
	}
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("second get: %v", err)
	}
	if dr.callCount() != 2 {
		t.Errorf("dial called %d times, want 2 (uncacheable token re-dialed)", dr.callCount())
	}
}

func TestPerIdentityConcurrent(t *testing.T) {
	ex := &fakeExchanger{tok: tokenexchange.Token{AccessToken: "exchanged", ExpiresAt: time.Unix(1<<40, 0)}}
	dr := &dialRecorder{}
	p := newPerIdentityBackend[*fakeBackend](ex, "backend-aud", dr.dial)
	configure(p, testID("alice"), true, testRaw, true, nil)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := p.get(context.Background()); err != nil {
				t.Errorf("get: %v", err)
			}
		})
	}
	wg.Wait()
}

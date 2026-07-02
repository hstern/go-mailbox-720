package kerbexchange

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
)

// stubReq records one call to the underlying exchange client.
type stubReq struct {
	token  string
	target kerb.ServicePrincipal
	output kerb.OutputType
}

// stubClient is a recording exchange client: it counts calls, captures each
// request, and returns a canned credential or error.
type stubClient struct {
	mu    sync.Mutex
	calls int
	reqs  []stubReq
	cred  *kerb.Credential
	err   error
}

func (s *stubClient) Exchange(_ context.Context, accessToken string, target kerb.ServicePrincipal, output kerb.OutputType) (*kerb.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.reqs = append(s.reqs, stubReq{token: accessToken, target: target, output: output})
	if s.err != nil {
		return nil, s.err
	}
	return s.cred, nil
}

func (s *stubClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func mustSPN(t *testing.T, s string) kerb.ServicePrincipal {
	t.Helper()
	sp, err := kerb.ParseServicePrincipal(s)
	if err != nil {
		t.Fatalf("ParseServicePrincipal(%q): %v", s, err)
	}
	return sp
}

// credAt builds a ccache credential expiring at the given time.
func credAt(t *testing.T, expiry time.Time) *kerb.Credential {
	t.Helper()
	return kerb.NewCredential("alice", mustSPN(t, "imap/mail.example.com@EXAMPLE.COM"), expiry, []byte("ccache-bytes"), nil)
}

func TestExchangePassesThrough(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)
	target := mustSPN(t, "imap/mail.example.com@EXAMPLE.COM")

	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(stub.reqs))
	}
	req := stub.reqs[0]
	if req.token != "user-token" {
		t.Errorf("token = %q, want %q", req.token, "user-token")
	}
	if req.target != target {
		t.Errorf("target = %v, want %v", req.target, target)
	}
	if req.output != kerb.OutputCCache {
		t.Errorf("output = %v, want %v", req.output, kerb.OutputCCache)
	}
}

func TestExchangeReturnsCredential(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	want := base.Add(time.Hour)
	stub := &stubClient{cred: credAt(t, want)}
	ex := newCaching(stub, WithClock(func() time.Time { return base }))

	cred, err := ex.Exchange(context.Background(), "user-token", mustSPN(t, "imap/h@R"), kerb.OutputCCache)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	cc, err := cred.CCache()
	if err != nil {
		t.Fatalf("CCache: %v", err)
	}
	if string(cc) != "ccache-bytes" {
		t.Errorf("ccache = %q, want %q", cc, "ccache-bytes")
	}
	if !cred.Expiry().Equal(want) {
		t.Errorf("Expiry = %v, want %v", cred.Expiry(), want)
	}
}

func TestExchangeCachesWithinTTL(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	stub := &stubClient{cred: credAt(t, base.Add(time.Hour))}
	ex := newCaching(stub, WithClock(func() time.Time { return now }))
	target := mustSPN(t, "imap/h@R")

	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	// Advance well within the lifetime (3600s) minus skew: still a hit.
	now = base.Add(30 * time.Minute)
	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("underlying client called %d times, want 1 (cache hit expected)", got)
	}
}

func TestExchangeReMintsNearExpiry(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	stub := &stubClient{cred: credAt(t, base.Add(time.Hour))}
	ex := newCaching(stub, WithClock(func() time.Time { return now }), WithSkew(60*time.Second))
	target := mustSPN(t, "imap/h@R")

	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	// Past (expiry - skew): 3600 - 60 = 3540s. Move to 3541s -> miss.
	now = base.Add(3541 * time.Second)
	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (re-mint near expiry)", got)
	}
}

func TestExchangeKeysOnTarget(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-token", mustSPN(t, "imap/a@R"), kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange a: %v", err)
	}
	if _, err := ex.Exchange(context.Background(), "user-token", mustSPN(t, "imap/b@R"), kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange b: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (distinct targets)", got)
	}
}

func TestExchangeKeysOnOutput(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)
	target := mustSPN(t, "imap/h@R")

	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange ccache: %v", err)
	}
	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputAPReq); err != nil {
		t.Fatalf("Exchange apreq: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (distinct output types)", got)
	}
}

func TestExchangeKeysOnSubjectToken(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)
	target := mustSPN(t, "imap/h@R")

	if _, err := ex.Exchange(context.Background(), "user-a", target, kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange user-a: %v", err)
	}
	if _, err := ex.Exchange(context.Background(), "user-b", target, kerb.OutputCCache); err != nil {
		t.Fatalf("Exchange user-b: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (distinct subject tokens)", got)
	}
}

func TestExchangeNoExpiryNotCached(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Time{})} // zero expiry: uncacheable
	ex := newCaching(stub)
	target := mustSPN(t, "imap/h@R")

	cred, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache)
	if err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if !cred.Expiry().IsZero() {
		t.Errorf("Expiry = %v, want zero", cred.Expiry())
	}
	if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (uncacheable credential re-minted)", got)
	}
}

func TestExchangeEmptySubjectToken(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "", mustSPN(t, "imap/h@R"), kerb.OutputCCache); err == nil {
		t.Fatal("Exchange with empty subject token: want error, got nil")
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("underlying client called %d times, want 0 (rejected before the wire)", got)
	}
}

func TestExchangePropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	stub := &stubClient{err: sentinel}
	ex := newCaching(stub)

	_, err := ex.Exchange(context.Background(), "user-token", mustSPN(t, "imap/h@R"), kerb.OutputCCache)
	if err == nil {
		t.Fatal("Exchange: want error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestExchangeRejectsNilCredential(t *testing.T) {
	// A nil credential with no error must be rejected, not cached or returned.
	stub := &stubClient{cred: nil}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-token", mustSPN(t, "imap/h@R"), kerb.OutputCCache); err == nil {
		t.Fatal("Exchange with nil credential: want error, got nil")
	}
}

func TestExchangeConcurrent(t *testing.T) {
	stub := &stubClient{cred: credAt(t, time.Now().Add(time.Hour))}
	ex := newCaching(stub)
	target := mustSPN(t, "imap/h@R")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := ex.Exchange(context.Background(), "user-token", target, kerb.OutputCCache); err != nil {
				t.Errorf("Exchange: %v", err)
			}
		})
	}
	wg.Wait()
}

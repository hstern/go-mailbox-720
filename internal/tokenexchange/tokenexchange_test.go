package tokenexchange

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	rfc8693 "github.com/hstern/go-token-exchange"
)

// stubClient is a recording go-token-exchange Client: it counts calls, captures
// each request, and returns a canned response or error.
type stubClient struct {
	mu    sync.Mutex
	calls int
	reqs  []*rfc8693.TokenExchangeRequest
	resp  *rfc8693.TokenExchangeResponse
	err   error
}

func (s *stubClient) Exchange(_ context.Context, req *rfc8693.TokenExchangeRequest) (*rfc8693.TokenExchangeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.reqs = append(s.reqs, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func (s *stubClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func okResponse() *rfc8693.TokenExchangeResponse {
	return &rfc8693.TokenExchangeResponse{
		AccessToken:     "backend-token",
		IssuedTokenType: rfc8693.TokenTypeAccessToken,
		TokenType:       "Bearer",
		ExpiresIn:       3600,
	}
}

func TestExchangeRequestShape(t *testing.T) {
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-token", "https://backend.example/jmap"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(stub.reqs))
	}
	req := stub.reqs[0]
	if req.GrantType != rfc8693.GrantTypeTokenExchange {
		t.Errorf("GrantType = %q, want %q", req.GrantType, rfc8693.GrantTypeTokenExchange)
	}
	if req.SubjectToken != "user-token" {
		t.Errorf("SubjectToken = %q, want %q", req.SubjectToken, "user-token")
	}
	if req.SubjectTokenType != rfc8693.TokenTypeAccessToken {
		t.Errorf("SubjectTokenType = %q, want %q", req.SubjectTokenType, rfc8693.TokenTypeAccessToken)
	}
	if req.RequestedTokenType != rfc8693.TokenTypeAccessToken {
		t.Errorf("RequestedTokenType = %q, want %q", req.RequestedTokenType, rfc8693.TokenTypeAccessToken)
	}
	if len(req.Audience) != 1 || req.Audience[0] != "https://backend.example/jmap" {
		t.Errorf("Audience = %v, want [https://backend.example/jmap]", req.Audience)
	}
}

func TestExchangeReturnsToken(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub, WithClock(func() time.Time { return base }))

	tok, err := ex.Exchange(context.Background(), "user-token", "aud")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.AccessToken != "backend-token" {
		t.Errorf("AccessToken = %q, want %q", tok.AccessToken, "backend-token")
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want %q", tok.TokenType, "Bearer")
	}
	want := base.Add(3600 * time.Second)
	if !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, want)
	}
}

func TestExchangeCachesWithinTTL(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub, WithClock(func() time.Time { return now }))

	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	// Advance well within the lifetime (3600s) minus skew: still a hit.
	now = base.Add(30 * time.Minute)
	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("underlying client called %d times, want 1 (cache hit expected)", got)
	}
}

func TestExchangeReExchangesNearExpiry(t *testing.T) {
	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	now := base
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub, WithClock(func() time.Time { return now }), WithSkew(60*time.Second))

	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	// Past (expiry - skew): 3600 - 60 = 3540s. Move to 3541s → miss.
	now = base.Add(3541 * time.Second)
	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (re-exchange near expiry)", got)
	}
}

func TestExchangeKeysOnAudience(t *testing.T) {
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-token", "aud-a"); err != nil {
		t.Fatalf("Exchange aud-a: %v", err)
	}
	if _, err := ex.Exchange(context.Background(), "user-token", "aud-b"); err != nil {
		t.Fatalf("Exchange aud-b: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (distinct audiences)", got)
	}
}

func TestExchangeKeysOnSubjectToken(t *testing.T) {
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-a", "aud"); err != nil {
		t.Fatalf("Exchange user-a: %v", err)
	}
	if _, err := ex.Exchange(context.Background(), "user-b", "aud"); err != nil {
		t.Fatalf("Exchange user-b: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (distinct subject tokens)", got)
	}
}

func TestExchangeNoLifetimeNotCached(t *testing.T) {
	resp := okResponse()
	resp.ExpiresIn = 0 // AS advertised no lifetime
	stub := &stubClient{resp: resp}
	ex := newCaching(stub)

	tok, err := ex.Exchange(context.Background(), "user-token", "aud")
	if err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want zero (no advertised lifetime)", tok.ExpiresAt)
	}
	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}
	if got := stub.callCount(); got != 2 {
		t.Errorf("underlying client called %d times, want 2 (uncacheable token re-exchanged)", got)
	}
}

func TestExchangeEmptySubjectToken(t *testing.T) {
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "", "aud"); err == nil {
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

	_, err := ex.Exchange(context.Background(), "user-token", "aud")
	if err == nil {
		t.Fatal("Exchange: want error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestExchangeRejectsInvalidResponse(t *testing.T) {
	// A response missing the required AccessToken must be rejected, not cached.
	stub := &stubClient{resp: &rfc8693.TokenExchangeResponse{
		IssuedTokenType: rfc8693.TokenTypeAccessToken,
		TokenType:       "Bearer",
		ExpiresIn:       3600,
	}}
	ex := newCaching(stub)

	if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err == nil {
		t.Fatal("Exchange with invalid response: want error, got nil")
	}
}

func TestExchangeConcurrent(t *testing.T) {
	stub := &stubClient{resp: okResponse()}
	ex := newCaching(stub)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := ex.Exchange(context.Background(), "user-token", "aud"); err != nil {
				t.Errorf("Exchange: %v", err)
			}
		})
	}
	wg.Wait()
}

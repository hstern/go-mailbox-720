package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/hstern/go-subjectid"
)

// mailboxSub asserts the identity is an IssSubID and returns its (iss, sub).
func mailboxSub(t *testing.T, id subjectid.SubjectIdentifier) (iss, sub string) {
	t.Helper()
	is, ok := id.(subjectid.IssSubID)
	if !ok {
		t.Fatalf("mailbox identity is %T, want subjectid.IssSubID", id)
	}
	return is.Iss, is.Sub
}

const (
	testKID      = "test-key"
	testAud      = "mailbox-api"
	testRSID     = "mailbox-rs"
	testRSSecret = "rs-secret"
)

// idp is a minimal OIDC issuer for tests: it serves discovery + JWKS and mints
// JWTs signed by its key.
type idp struct {
	issuer string
	key    *rsa.PrivateKey
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &idp{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                i.issuer,
			"jwks_uri":                              i.issuer + "/jwks",
			"authorization_endpoint":                i.issuer + "/authorize",
			"token_endpoint":                        i.issuer + "/token",
			"introspection_endpoint":                i.issuer + "/introspect",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	// RFC 7662 introspection: authenticates the RS by its client credentials and
	// reports the opaque token "opaque-active" as active.
	mux.HandleFunc("/introspect", func(w http.ResponseWriter, r *http.Request) {
		if id, secret, _ := r.BasicAuth(); id != testRSID || secret != testRSSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := map[string]any{"active": false}
		switch r.FormValue("token") {
		case "opaque-active": // Kanidm-style: no aud member
			resp = map[string]any{"active": true, "scope": "Mail.Read", "sub": "svc@example.com"}
		case "opaque-good-aud":
			resp = map[string]any{"active": true, "aud": testAud, "scope": "Mail.Read", "sub": "svc@example.com"}
		case "opaque-wrong-aud":
			resp = map[string]any{"active": true, "aud": "someone-else", "scope": "Mail.Read", "sub": "svc@example.com"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: testKID, Algorithm: "RS256", Use: "sig",
		}}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	i.issuer = srv.URL
	return i
}

// sign mints a plain (typ=JWT) bearer token; signAT mints an RFC 9068 access token
// (typ=at+jwt), which the middleware holds to the full profile.
func (i *idp) sign(t *testing.T, claims map[string]any) string {
	return signWith(t, i.key, testKID, "JWT", claims)
}

func (i *idp) signAT(t *testing.T, claims map[string]any) string {
	return signWith(t, i.key, testKID, "at+jwt", claims)
}

func signWith(t *testing.T, key *rsa.PrivateKey, kid, typ string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// baseClaims is a plain (non-RFC-9068) JWT access token of the shape Microsoft
// Entra and many IdPs emit: typ=JWT, scopes in scp, no jti/client_id. The
// middleware accepts it on signature + audience; the RFC 9068 profile is enforced
// only for typ=at+jwt tokens (see the at+jwt cases).
func baseClaims(iss string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": iss,
		"aud": testAud,
		"sub": "user@example.com",
		"scp": "Mail.Read",
		"iat": now.Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func TestMiddleware(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		SubjectClaim:   "sub",
		ScopeClaims:    []string{"scope", "scp", "roles"}, // Entra-style: read scp
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		token       func() string // "" => no Authorization header
		wantStatus  int
		wantMailbox string // non-empty only on the accepted (200) case
	}{
		// A plain typ=JWT token (Microsoft-style: scp scopes, no jti/client_id) is
		// accepted on signature + audience.
		{"valid plain jwt", func() string { return idp.sign(t, baseClaims(idp.issuer)) }, http.StatusOK, "user@example.com"},
		{"scope via roles", func() string {
			c := baseClaims(idp.issuer)
			delete(c, "scp")
			c["roles"] = []any{"Mail.Read", "other"}
			return idp.sign(t, c)
		}, http.StatusOK, "user@example.com"},
		// An RFC 9068 access token (typ=at+jwt) IS held to the profile: complete →
		// accepted; missing the required jti/client_id → rejected.
		{"at+jwt conformant", func() string {
			c := baseClaims(idp.issuer)
			delete(c, "scp")
			c["scope"] = "Mail.Read"
			c["jti"] = "jti-1"
			c["client_id"] = "test-client"
			return idp.signAT(t, c)
		}, http.StatusOK, "user@example.com"},
		{"at+jwt missing required claims", func() string {
			return idp.signAT(t, baseClaims(idp.issuer)) // no jti/client_id
		}, http.StatusUnauthorized, ""},
		{"missing header", func() string { return "" }, http.StatusUnauthorized, ""},
		{"malformed token", func() string { return "not.a.jwt" }, http.StatusUnauthorized, ""},
		{"untrusted issuer", func() string {
			c := baseClaims(idp.issuer)
			c["iss"] = "https://evil.example"
			return idp.sign(t, c)
		}, http.StatusUnauthorized, ""},
		{"bad signature", func() string { return signWith(t, otherKey, testKID, "JWT", baseClaims(idp.issuer)) }, http.StatusUnauthorized, ""},
		{"wrong audience", func() string {
			c := baseClaims(idp.issuer)
			c["aud"] = "someone-else"
			return idp.sign(t, c)
		}, http.StatusUnauthorized, ""},
		{"expired", func() string {
			c := baseClaims(idp.issuer)
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return idp.sign(t, c)
		}, http.StatusUnauthorized, ""},
		{"missing scope", func() string {
			c := baseClaims(idp.issuer)
			c["scp"] = "openid"
			return idp.sign(t, c)
		}, http.StatusForbidden, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ranNext bool
			var gotID subjectid.SubjectIdentifier
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ranNext = true
				gotID, _ = Mailbox(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
			if tok := tc.token(); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			a.Middleware(next).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if !ranNext {
					t.Error("next handler did not run for an accepted request")
				}
				iss, sub := mailboxSub(t, gotID)
				if sub != tc.wantMailbox {
					t.Errorf("mailbox sub = %q, want %q", sub, tc.wantMailbox)
				}
				if iss != idp.issuer {
					t.Errorf("mailbox iss = %q, want %q", iss, idp.issuer)
				}
			} else if ranNext {
				t.Error("next handler ran for a rejected request")
			}
		})
	}
}

func TestRawTokenInContext(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		ScopeClaims:    []string{"scope", "scp", "roles"},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("set on accepted request", func(t *testing.T) {
		tok := idp.sign(t, baseClaims(idp.issuer))
		var gotRaw string
		var gotOK bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotRaw, gotOK = RawToken(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		a.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

		if !gotOK {
			t.Fatal("RawToken not present on an accepted request")
		}
		if gotRaw != tok {
			t.Errorf("RawToken = %q, want the presented bearer token %q", gotRaw, tok)
		}
	})

	t.Run("absent on rejected request", func(t *testing.T) {
		// next must not run for a rejected request, but guard anyway: a rejected
		// token must never expose a raw token downstream.
		var gotOK bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, gotOK = RawToken(r.Context())
		})
		req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
		req.Header.Set("Authorization", "Bearer not-a-valid-token")
		rec := httptest.NewRecorder()
		a.Middleware(next).ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Fatal("invalid token was accepted")
		}
		if gotOK {
			t.Error("RawToken present on a rejected request")
		}
	})
}

// RawToken on a context the middleware never touched reports absent.
func TestRawTokenAbsentByDefault(t *testing.T) {
	if raw, ok := RawToken(context.Background()); ok || raw != "" {
		t.Errorf("RawToken on a bare context = (%q, %v), want (\"\", false)", raw, ok)
	}
}

func TestSubjectClaimMapping(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:      []string{idp.issuer},
		Audience:     testAud,
		SubjectClaim: "preferred_username",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c := baseClaims(idp.issuer)
	c["preferred_username"] = "alice"

	var gotID subjectid.SubjectIdentifier
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotID, _ = Mailbox(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
	req.Header.Set("Authorization", "Bearer "+idp.sign(t, c))
	a.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if _, sub := mailboxSub(t, gotID); sub != "alice" {
		t.Errorf("mailbox sub = %q, want alice (mapped from preferred_username)", sub)
	}
}

func TestMiddlewareIntrospection(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		SubjectClaim:   "sub",
		Introspection:  &IntrospectionConfig{ClientID: testRSID, ClientSecret: testRSSecret},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name        string
		token       string
		wantStatus  int
		wantMailbox string
	}{
		{"active opaque token (no aud)", "opaque-active", http.StatusOK, "svc@example.com"},
		{"active with matching aud", "opaque-good-aud", http.StatusOK, "svc@example.com"},
		{"active with wrong aud", "opaque-wrong-aud", http.StatusUnauthorized, ""},
		{"inactive opaque token", "opaque-bogus", http.StatusUnauthorized, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ranNext bool
			var gotID subjectid.SubjectIdentifier
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ranNext = true
				gotID, _ = Mailbox(r.Context())
			})
			req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			a.Middleware(next).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if !ranNext {
					t.Error("next handler did not run for an accepted token")
				}
				if _, sub := mailboxSub(t, gotID); sub != tc.wantMailbox {
					t.Errorf("mailbox sub = %q, want %q", sub, tc.wantMailbox)
				}
			} else if ranNext {
				t.Error("next handler ran for a rejected token")
			}
		})
	}
}

// stubRevocations is a RevocationChecker that revokes a fixed set of (subject)
// and (jti) keys, recording the arguments the middleware passed for assertion.
type stubRevocations struct {
	revokedSubs map[subjectid.IssSubID]bool
	revokedJTIs map[string]bool
	gotIssuedAt time.Time
	gotJTI      string
}

func (s *stubRevocations) Revoked(sub subjectid.IssSubID, issuedAt time.Time, jti string) bool {
	s.gotIssuedAt = issuedAt
	s.gotJTI = jti
	return s.revokedSubs[sub] || s.revokedJTIs[jti]
}

// A token that validates but whose subject (or jti) a Shared Signals event has
// revoked is rejected 401; a non-revoked token still passes, and the middleware
// hands the token's iat + jti to the checker.
func TestMiddlewareRevocation(t *testing.T) {
	idp := newIDP(t)
	rev := &stubRevocations{
		revokedSubs: map[subjectid.IssSubID]bool{{Iss: idp.issuer, Sub: "revoked@example.com"}: true},
		revokedJTIs: map[string]bool{"dead-jti": true},
	}
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		ScopeClaims:    []string{"scope", "scp", "roles"},
		Revocations:    rev,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name       string
		claims     func() map[string]any
		wantStatus int
	}{
		{"revoked subject is rejected", func() map[string]any {
			c := baseClaims(idp.issuer)
			c["sub"] = "revoked@example.com"
			c["jti"] = "live-jti"
			return c
		}, http.StatusUnauthorized},
		{"revoked jti is rejected", func() map[string]any {
			c := baseClaims(idp.issuer)
			c["sub"] = "ok@example.com"
			c["jti"] = "dead-jti"
			return c
		}, http.StatusUnauthorized},
		{"non-revoked token passes", func() map[string]any {
			c := baseClaims(idp.issuer)
			c["sub"] = "ok@example.com"
			c["jti"] = "live-jti"
			return c
		}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ranNext bool
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				ranNext = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
			req.Header.Set("Authorization", "Bearer "+idp.sign(t, tc.claims()))
			rec := httptest.NewRecorder()
			a.Middleware(next).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if (tc.wantStatus == http.StatusOK) != ranNext {
				t.Errorf("ranNext = %v, want %v", ranNext, tc.wantStatus == http.StatusOK)
			}
		})
	}

	// The iat the IdP minted (baseClaims sets it to ~now) and the jti reached the
	// checker, so the session-revoked (issued-at-or-before T) path has real inputs.
	if rev.gotIssuedAt.IsZero() {
		t.Error("middleware passed a zero issuedAt to the revocation checker; iat was not propagated")
	}
	if rev.gotJTI == "" {
		t.Error("middleware passed an empty jti to the revocation checker")
	}
}

func TestNewRequiresIssuer(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Error("New with no issuers should error")
	}
}

func TestNewFailsOnUndiscoverableIssuer(t *testing.T) {
	// An issuer whose discovery endpoint 404s must fail New, not be trusted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no discovery here", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := New(context.Background(), Config{Issuers: []string{srv.URL}}); err == nil {
		t.Error("New should error when an issuer cannot be discovered")
	}
}

// With the default ScopeClaims (scope, roles), a token that carries its scope only
// in the non-standard "scp" claim does NOT satisfy RequiredScopes — scp is opt-in
// for Entra deployments, not read by default.
func TestScopeClaimsDefaultDoesNotReadScp(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		// ScopeClaims omitted -> defaults to ["scope", "roles"].
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var ran bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true })
	req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
	req.Header.Set("Authorization", "Bearer "+idp.sign(t, baseClaims(idp.issuer))) // scope in scp
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (scp not a default scope claim)", rec.Code)
	}
	if ran {
		t.Error("next ran despite the scope being only in scp under default ScopeClaims")
	}
}

// The auth failures emit an RFC 6750 §3 WWW-Authenticate: Bearer challenge — a bare
// one for missing credentials, error=invalid_token for a bad token, and
// error=insufficient_scope (with the required scope) for a scope failure.
func TestWWWAuthenticateChallenge(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		ScopeClaims:    []string{"scope", "scp", "roles"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		auth   string // Authorization header value ("" => none)
		want   int
		header string // expected WWW-Authenticate value
	}{
		{"no credentials", "", http.StatusUnauthorized, `Bearer realm="mailbox-api"`},
		{"bad token", "Bearer " + signWith(t, otherKey, testKID, "JWT", baseClaims(idp.issuer)), http.StatusUnauthorized, `Bearer realm="mailbox-api", error="invalid_token"`},
		{"insufficient scope", "Bearer " + idp.sign(t, func() map[string]any { c := baseClaims(idp.issuer); c["scp"] = "openid"; return c }()), http.StatusForbidden, `Bearer realm="mailbox-api", error="insufficient_scope", scope="Mail.Read"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != tc.header {
				t.Errorf("WWW-Authenticate = %q, want %q", got, tc.header)
			}
		})
	}
}

// With a ResourceID configured, the WWW-Authenticate challenge carries the RFC 9728
// §5.1 resource_metadata parameter pointing at the protected-resource metadata.
func TestChallengeIncludesResourceMetadata(t *testing.T) {
	idp := newIDP(t)
	a, err := New(context.Background(), Config{
		Issuers:        []string{idp.issuer},
		Audience:       testAud,
		RequiredScopes: []string{"Mail.Read"},
		ResourceID:     "https://mailbox.example.com",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1.0/me/messages", nil)) // no token

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `resource_metadata="https://mailbox.example.com/.well-known/oauth-protected-resource"`
	if !strings.Contains(got, want) {
		t.Errorf("WWW-Authenticate = %q, want to contain %q", got, want)
	}
}

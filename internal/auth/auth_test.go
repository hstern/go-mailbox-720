package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func (i *idp) sign(t *testing.T, claims map[string]any) string {
	return signWith(t, i.key, testKID, claims)
}

func signWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
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
		{"valid", func() string { return idp.sign(t, baseClaims(idp.issuer)) }, http.StatusOK, "user@example.com"},
		{"scope via roles", func() string {
			c := baseClaims(idp.issuer)
			delete(c, "scp")
			c["roles"] = []any{"Mail.Read", "other"}
			return idp.sign(t, c)
		}, http.StatusOK, "user@example.com"},
		{"missing header", func() string { return "" }, http.StatusUnauthorized, ""},
		{"malformed token", func() string { return "not.a.jwt" }, http.StatusUnauthorized, ""},
		{"untrusted issuer", func() string {
			c := baseClaims(idp.issuer)
			c["iss"] = "https://evil.example"
			return idp.sign(t, c)
		}, http.StatusUnauthorized, ""},
		{"bad signature", func() string { return signWith(t, otherKey, testKID, baseClaims(idp.issuer)) }, http.StatusUnauthorized, ""},
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

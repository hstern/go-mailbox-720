// jwksvalidator_test.go is the offline (JWKS) counterpart to the RFC 7662
// introspection tokenValidator. When mailboxd runs with
// -tokenexchange-requested-token-type=jwt (MB720-56), Zitadel issues the
// exchanged backend token as a JWS JWT instead of an opaque JWE — so a backend
// can validate it by verifying the signature against the issuer's JWKS and
// checking its audience, with NO per-request introspection round-trip. This
// validator proves that path; it satisfies the same bearerValidator interface
// the impersonation fakes use, so a JWT-mode test swaps it in for tokenValidator.
//
// Verification is stdlib-only (RS256 = RSASSA-PKCS1-v1.5 over SHA-256), matching
// the e2e module's no-extra-deps rule.
package e2e

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jwksValidator validates an exchanged backend token offline: it verifies the
// JWS signature against the issuer's JWKS and requires the token to carry a
// specific backend audience. It is the no-introspection path MB720-56's
// requested_token_type=jwt enables, and satisfies bearerValidator so an
// impersonation fake can use it in place of the RFC 7662 tokenValidator.
type jwksValidator struct {
	jwksURL    string // issuer's JWKS endpoint (RFC 7517)
	issuer     string // required iss ("" skips the check)
	backendAud string // required backend audience (a resolved Zitadel project id)
	client     *http.Client
	now        func() time.Time
}

// newJWKSValidatorAt builds a validator against an explicit JWKS URL. Used
// directly by the offline unit test; the e2e path uses newJWKSValidator.
func newJWKSValidatorAt(jwksURL, issuer, backendAud string, client *http.Client) *jwksValidator {
	if client == nil {
		client = http.DefaultClient
	}
	return &jwksValidator{
		jwksURL:    jwksURL,
		issuer:     issuer,
		backendAud: backendAud,
		client:     client,
		now:        time.Now,
	}
}

// newJWKSValidator discovers the issuer's jwks_uri from its OIDC discovery
// document and returns a validator requiring backendAud in the token's aud.
func newJWKSValidator(t *testing.T, z *zitadelIDP, backendAud string) *jwksValidator {
	t.Helper()
	client := httpClientFor(z)
	resp, err := client.Get(z.issuer() + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("fetch OIDC discovery: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var d struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode OIDC discovery: %v", err)
	}
	if d.JWKSURI == "" {
		t.Fatal("OIDC discovery has no jwks_uri")
	}
	return newJWKSValidatorAt(d.JWKSURI, z.issuer(), backendAud, client)
}

// validate verifies bearer as an RS256 JWS signed by a key in the issuer's JWKS
// and returns its sub only when the signature is valid, the token is unexpired,
// the iss matches (when required), and the aud carries the backend audience.
// stdlib-only: no JWT/JOSE dependency.
func (v *jwksValidator) validate(bearer string) (sub string, err error) {
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("jwks: not a JWS (%d segments)", len(parts))
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return "", fmt.Errorf("jwks: decode header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return "", fmt.Errorf("jwks: unexpected alg %q (want RS256)", hdr.Alg)
	}

	pub, err := v.publicKey(hdr.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("jwks: decode signature: %w", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return "", fmt.Errorf("jwks: signature verification failed: %w", err)
	}

	var claims struct {
		Sub string  `json:"sub"`
		Iss string  `json:"iss"`
		Exp float64 `json:"exp"`
		Aud any     `json:"aud"` // string or []string per RFC 7519
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", fmt.Errorf("jwks: decode claims: %w", err)
	}
	if claims.Exp > 0 && v.now().After(time.Unix(int64(claims.Exp), 0)) {
		return "", fmt.Errorf("jwks: token expired")
	}
	if v.issuer != "" && claims.Iss != v.issuer {
		return "", fmt.Errorf("jwks: iss %q != expected %q", claims.Iss, v.issuer)
	}
	if !audClaimContains(claims.Aud, v.backendAud) {
		return "", fmt.Errorf("jwks: token lacks required backend audience %q", v.backendAud)
	}
	return claims.Sub, nil
}

// publicKey fetches the JWKS and returns the RSA public key for kid (or the only
// key when the JWKS has exactly one and kid is empty).
func (v *jwksValidator) publicKey(kid string) (*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("jwks: fetch keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: keys endpoint returned %d: %s", resp.StatusCode, body)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("jwks: decode keys: %w", err)
	}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if kid != "" && k.Kid != kid {
			continue
		}
		if kid == "" && len(set.Keys) != 1 {
			break // ambiguous: a kid is required to disambiguate a multi-key set
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("jwks: decode modulus: %w", err)
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("jwks: decode exponent: %w", err)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}, nil
	}
	return nil, fmt.Errorf("jwks: no RSA key for kid %q", kid)
}

// decodeSegment base64url-decodes a JWS segment and JSON-unmarshals it into out.
func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// audClaimContains reports whether the RFC 7519 aud claim (a string or []string)
// contains want.
func audClaimContains(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// signRS256 builds a compact JWS (header.payload.signature) signed with key,
// stamping the given kid in the header. Test-only helper.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// startJWKSServer serves a single-key JWKS (RFC 7517) for key under kid and
// returns its URL. Test-only helper.
func startJWKSServer(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	jwk := map[string]any{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
	body, err := json.Marshal(map[string]any{"keys": []any{jwk}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestJWKSValidator(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid, aud, iss, sub = "k1", "backend-aud", "https://issuer.example", "user-sub-123"
	jwksURL := startJWKSServer(t, key, kid)
	v := newJWKSValidatorAt(jwksURL, iss, aud, http.DefaultClient)

	exp := float64(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix())

	t.Run("accepts a valid token and returns sub", func(t *testing.T) {
		tok := signRS256(t, key, kid, map[string]any{"sub": sub, "aud": []any{aud}, "iss": iss, "exp": exp})
		got, err := v.validate(tok)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if got != sub {
			t.Errorf("sub = %q, want %q", got, sub)
		}
	})

	t.Run("rejects a token missing the backend audience", func(t *testing.T) {
		tok := signRS256(t, key, kid, map[string]any{"sub": sub, "aud": []any{"other-aud"}, "iss": iss, "exp": exp})
		if _, err := v.validate(tok); err == nil {
			t.Fatal("want error for wrong audience, got nil")
		}
	})

	t.Run("rejects a token signed by an unknown key", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		tok := signRS256(t, other, kid, map[string]any{"sub": sub, "aud": []any{aud}, "iss": iss, "exp": exp})
		if _, err := v.validate(tok); err == nil {
			t.Fatal("want error for bad signature, got nil")
		}
	})

	t.Run("rejects an expired token", func(t *testing.T) {
		past := float64(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
		tok := signRS256(t, key, kid, map[string]any{"sub": sub, "aud": []any{aud}, "iss": iss, "exp": past})
		if _, err := v.validate(tok); err == nil {
			t.Fatal("want error for expired token, got nil")
		}
	})

	t.Run("rejects an opaque (non-JWT) token", func(t *testing.T) {
		if _, err := v.validate("not-a-jwt"); err == nil {
			t.Fatal("want error for non-JWT token, got nil")
		}
	})

	t.Run("rejects an alg:none token", func(t *testing.T) {
		// The classic JWS bypass: a well-formed header/payload with alg:none and an
		// empty signature. The validator must reject it on the algorithm check before
		// ever attempting verification.
		enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
		hdr := enc(`{"alg":"none","typ":"JWT","kid":"` + kid + `"}`)
		pay := enc(`{"sub":"` + sub + `","aud":["` + aud + `"],"iss":"` + iss + `"}`)
		if _, err := v.validate(hdr + "." + pay + "."); err == nil {
			t.Fatal("want error for alg:none token, got nil")
		}
	})
}

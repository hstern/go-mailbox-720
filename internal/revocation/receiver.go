package revocation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	ssf "github.com/hstern/go-ssf"
	"github.com/hstern/go-ssf/receiver"
)

// Handler builds the RFC 8935 push-delivery endpoint for Shared Signals SETs. It
// fetches each issuer's JWKS (via OIDC discovery), combines every issuer's keys
// into one key set, and returns a receiver.PushHandler that verifies an incoming
// SET's JWS against that set before handing the verified payload to sink.
//
// One verifier spans the union of all issuers' keys; the JWS "kid" (or, failing
// that, trial verification) selects the right key. The SET signature is the only
// gate on this endpoint — it carries no separate authentication — so the handler
// is mounted unauthenticated by the caller.
//
// Handler errors if no issuers are configured or any issuer's JWKS cannot be
// fetched (fail-closed: an un-fetchable issuer must not silently weaken the set).
func Handler(issuers []string, sink receiver.Sink) (http.Handler, error) {
	if len(issuers) == 0 {
		return nil, fmt.Errorf("revocation: no issuers configured")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var combined jose.JSONWebKeySet
	for _, iss := range issuers {
		keys, err := fetchJWKS(ctx, client, iss)
		if err != nil {
			return nil, fmt.Errorf("revocation: fetch JWKS for issuer %q: %w", iss, err)
		}
		combined.Keys = append(combined.Keys, keys.Keys...)
	}
	if len(combined.Keys) == 0 {
		return nil, fmt.Errorf("revocation: no signing keys across %d issuer(s)", len(issuers))
	}
	verifier := ssf.NewJOSESetVerifier(combined)
	return receiver.PushHandler(verifier, sink), nil
}

// fetchJWKS resolves an issuer's jwks_uri through OIDC discovery and returns the
// fetched key set.
func fetchJWKS(ctx context.Context, client *http.Client, issuer string) (jose.JSONWebKeySet, error) {
	var empty jose.JSONWebKeySet

	var disco struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, client, issuer+"/.well-known/openid-configuration", &disco); err != nil {
		return empty, err
	}
	if disco.JWKSURI == "" {
		return empty, fmt.Errorf("discovery document has no jwks_uri")
	}

	var keys jose.JSONWebKeySet
	if err := getJSON(ctx, client, disco.JWKSURI, &keys); err != nil {
		return empty, err
	}
	return keys, nil
}

// getJSON GETs url and decodes a JSON body into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

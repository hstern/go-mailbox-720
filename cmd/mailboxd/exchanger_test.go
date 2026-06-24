package main

import (
	"testing"

	rfc8693 "github.com/hstern/go-token-exchange"
)

func TestBuildExchangerDisabled(t *testing.T) {
	// With no endpoint, token exchange is disabled: a valid (default)
	// requested-token-type yields no exchanger and no error...
	ex, err := buildExchanger("", "", "client_secret_basic", rfc8693.TokenTypeAccessToken)
	if err != nil {
		t.Fatalf("buildExchanger(disabled): unexpected error: %v", err)
	}
	if ex != nil {
		t.Errorf("buildExchanger(disabled): got %v, want nil exchanger", ex)
	}

	// ...but a bad requested-token-type is rejected even when disabled, so the
	// typo surfaces at startup rather than only once an endpoint is added.
	if _, err := buildExchanger("", "", "client_secret_basic", "urn:ietf:params:oauth:token-type:saml2"); err == nil {
		t.Error("buildExchanger(disabled, bad requested-token-type): want error, got nil")
	}
}

func TestRequestedTokenType(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"default access token", rfc8693.TokenTypeAccessToken, rfc8693.TokenTypeAccessToken, false},
		{"jwt", rfc8693.TokenTypeJWT, rfc8693.TokenTypeJWT, false},
		{"unsupported uri", rfc8693.TokenTypeIDToken, "", true},
		{"not a uri", "jwt", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requestedTokenType(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("requestedTokenType(%q): want error, got nil (got %q)", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("requestedTokenType(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("requestedTokenType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

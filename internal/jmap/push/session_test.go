package push

import (
	"encoding/json"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

func sessionWithRawCaps(t *testing.T, caps map[gojmap.URI]string) *gojmap.Session {
	t.Helper()
	raw := map[gojmap.URI]json.RawMessage{}
	for uri, body := range caps {
		raw[uri] = json.RawMessage(body)
	}
	return &gojmap.Session{RawCapabilities: raw}
}

func TestWebSocketURL(t *testing.T) {
	tests := []struct {
		name     string
		caps     map[gojmap.URI]string
		wantOK   bool
		wantURL  string
		wantPush bool
	}{
		{
			name:     "present with push",
			caps:     map[gojmap.URI]string{CapabilityURI: `{"url":"wss://jmap.example/ws","supportsPush":true}`},
			wantOK:   true,
			wantURL:  "wss://jmap.example/ws",
			wantPush: true,
		},
		{
			name:    "present without push",
			caps:    map[gojmap.URI]string{CapabilityURI: `{"url":"wss://jmap.example/ws","supportsPush":false}`},
			wantOK:  true,
			wantURL: "wss://jmap.example/ws",
		},
		{
			name:   "capability absent",
			caps:   map[gojmap.URI]string{"urn:ietf:params:jmap:core": `{}`},
			wantOK: false,
		},
		{
			name:   "malformed (no url)",
			caps:   map[gojmap.URI]string{CapabilityURI: `{"supportsPush":true}`},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := WebSocketURL(sessionWithRawCaps(t, tc.caps))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tc.wantURL)
			}
			if got.SupportsPush != tc.wantPush {
				t.Errorf("SupportsPush = %v, want %v", got.SupportsPush, tc.wantPush)
			}
		})
	}
}

func TestWebSocketURLNilSession(t *testing.T) {
	if _, ok := WebSocketURL(nil); ok {
		t.Fatal("WebSocketURL(nil) returned ok=true")
	}
}

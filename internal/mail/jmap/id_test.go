package jmap

import (
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

func TestFolderIDRoundTrip(t *testing.T) {
	for _, id := range []gojmap.ID{"INBOX", "mbx-1", "a/b/c", ""} {
		got, err := decodeFolderID(folderID(id))
		if err != nil {
			t.Fatalf("decodeFolderID(%q): %v", id, err)
		}
		if got != id {
			t.Errorf("folder id round trip: got %q, want %q", got, id)
		}
	}
}

func TestMessageIDRoundTrip(t *testing.T) {
	for _, id := range []gojmap.ID{"email-42", "Zm9v", ""} {
		got, err := decodeMessageID(messageID(id))
		if err != nil {
			t.Fatalf("decodeMessageID(%q): %v", id, err)
		}
		if got != id {
			t.Errorf("message id round trip: got %q, want %q", got, id)
		}
	}
}

func TestDecodeIDsRejectGarbage(t *testing.T) {
	for _, bad := range []string{"not-base64!!", "%%%"} {
		if _, err := decodeFolderID(bad); err == nil {
			t.Errorf("decodeFolderID(%q) = nil error, want error", bad)
		}
		if _, err := decodeMessageID(bad); err == nil {
			t.Errorf("decodeMessageID(%q) = nil error, want error", bad)
		}
	}
}

func TestDeltaTokenRoundTrip(t *testing.T) {
	for _, state := range []string{"state-1", "abc123", ""} {
		got, err := decodeDeltaToken(encodeDeltaToken(state))
		if err != nil {
			t.Fatalf("decodeDeltaToken(%q): %v", state, err)
		}
		if got != state {
			t.Errorf("delta token round trip: got %q, want %q", got, state)
		}
	}
}

func TestDecodeDeltaTokenRejectsGarbage(t *testing.T) {
	if _, err := decodeDeltaToken("not base64 !!"); err == nil {
		t.Error("decodeDeltaToken(garbage) = nil error, want error")
	}
}

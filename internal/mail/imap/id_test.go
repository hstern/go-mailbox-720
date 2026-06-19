package imap

import "testing"

func TestFolderIDRoundTrip(t *testing.T) {
	for _, name := range []string{"INBOX", "Archive/2024", "Lists.golang", ""} {
		got, err := decodeFolderID(folderID(name))
		if err != nil {
			t.Fatalf("decodeFolderID(%q): %v", name, err)
		}
		if got != name {
			t.Errorf("folder id round trip: got %q, want %q", got, name)
		}
	}
}

func TestMessageIDRoundTrip(t *testing.T) {
	mailbox, uidValidity, uid := "Archive/2024", uint32(1234567890), uint32(42)
	gotM, gotV, gotU, err := decodeMessageID(messageID(mailbox, uidValidity, uid))
	if err != nil {
		t.Fatalf("decodeMessageID: %v", err)
	}
	if gotM != mailbox || gotV != uidValidity || gotU != uid {
		t.Errorf("got (%q,%d,%d), want (%q,%d,%d)", gotM, gotV, gotU, mailbox, uidValidity, uid)
	}
}

func TestDecodeMessageIDRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-base64!!", "", "Zm9v"} { // last decodes but lacks the 3-part structure
		if _, _, _, err := decodeMessageID(bad); err == nil {
			t.Errorf("decodeMessageID(%q) = nil error, want error", bad)
		}
	}
}

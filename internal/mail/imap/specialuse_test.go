package imap

import (
	"context"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func TestWellKnownName(t *testing.T) {
	tests := []struct {
		name    string
		mailbox string
		attrs   []goimap.MailboxAttr
		want    string
	}{
		{"inbox by name", "INBOX", nil, "inbox"},
		{"inbox case-insensitive", "Inbox", nil, "inbox"},
		{"sent", "Sent", []goimap.MailboxAttr{goimap.MailboxAttrSent}, "sentitems"},
		{"drafts", "Drafts", []goimap.MailboxAttr{goimap.MailboxAttrDrafts}, "drafts"},
		{"trash", "Trash", []goimap.MailboxAttr{goimap.MailboxAttrTrash}, "deleteditems"},
		{"junk", "Spam", []goimap.MailboxAttr{goimap.MailboxAttrJunk}, "junkemail"},
		{"archive", "Archive", []goimap.MailboxAttr{goimap.MailboxAttrArchive}, "archive"},
		{"all has no graph equivalent", "All Mail", []goimap.MailboxAttr{goimap.MailboxAttrAll}, ""},
		{"flagged has no graph equivalent", "Flagged", []goimap.MailboxAttr{goimap.MailboxAttrFlagged}, ""},
		{"ordinary folder", "Projects", nil, ""},
		{"first recognized special-use wins", "X", []goimap.MailboxAttr{goimap.MailboxAttrArchive, goimap.MailboxAttrSent}, "archive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wellKnownName(tc.mailbox, tc.attrs); got != tc.want {
				t.Errorf("wellKnownName(%q, %v) = %q, want %q", tc.mailbox, tc.attrs, got, tc.want)
			}
		})
	}
}

// TestListMailFoldersInboxWellKnown checks the LIST→folder wiring populates the
// INBOX's WellKnownName end-to-end against the in-memory server (which has an
// INBOX but no SPECIAL-USE folders, so INBOX-by-name is what it can exercise).
func TestListMailFoldersInboxWellKnown(t *testing.T) {
	cl := dialTest(t)
	folders, err := cl.ListMailFolders(context.Background())
	if err != nil {
		t.Fatalf("ListMailFolders: %v", err)
	}
	for _, f := range folders {
		if f.DisplayName == "INBOX" {
			if f.WellKnownName != "inbox" {
				t.Errorf("INBOX WellKnownName = %q, want %q", f.WellKnownName, "inbox")
			}
			return
		}
	}
	t.Fatalf("INBOX not found in %+v", folders)
}

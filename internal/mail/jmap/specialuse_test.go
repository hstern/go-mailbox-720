package jmap

import (
	"testing"

	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
)

func TestWellKnownName(t *testing.T) {
	tests := []struct {
		role mailbox.Role
		want string
	}{
		{mailbox.RoleInbox, "inbox"},
		{mailbox.RoleSent, "sentitems"},
		{mailbox.RoleDrafts, "drafts"},
		{mailbox.RoleTrash, "deleteditems"},
		{mailbox.RoleJunk, "junkemail"},
		{mailbox.RoleArchive, "archive"},
		{mailbox.RoleAll, ""},
		{mailbox.RoleFlagged, ""},
		{mailbox.Role(""), ""},
	}
	for _, tc := range tests {
		if got := wellKnownName(tc.role); got != tc.want {
			t.Errorf("wellKnownName(%q) = %q, want %q", tc.role, got, tc.want)
		}
	}
}

package jmap

import "git.sr.ht/~rockorager/go-jmap/mail/mailbox"

// wellKnownName maps a JMAP mailbox role (RFC 8621) onto the Graph well-known
// folder name the server resolves path aliases to ("inbox", "sentitems",
// "drafts", "deleteditems", "junkemail", "archive"), or "" for a role with no
// Graph well-known equivalent (all, flagged, important) or no role at all.
func wellKnownName(role mailbox.Role) string {
	switch role {
	case mailbox.RoleInbox:
		return "inbox"
	case mailbox.RoleSent:
		return "sentitems"
	case mailbox.RoleDrafts:
		return "drafts"
	case mailbox.RoleTrash:
		return "deleteditems"
	case mailbox.RoleJunk:
		return "junkemail"
	case mailbox.RoleArchive:
		return "archive"
	default:
		return ""
	}
}

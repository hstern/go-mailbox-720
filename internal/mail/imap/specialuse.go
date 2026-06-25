package imap

import (
	"strings"

	goimap "github.com/emersion/go-imap/v2"
)

// wellKnownName maps an IMAP mailbox onto the Graph well-known folder name the
// server resolves path aliases to ("inbox", "sentitems", "drafts",
// "deleteditems", "junkemail", "archive"), or "" for an ordinary folder.
//
// INBOX is identified by name (it has no SPECIAL-USE attribute — RFC 3501 makes
// it special by name); the rest come from the RFC 6154 SPECIAL-USE attributes a
// server reports in LIST. Attributes with no Graph well-known equivalent (\All,
// \Flagged, \Important) yield "".
func wellKnownName(mailbox string, attrs []goimap.MailboxAttr) string {
	if strings.EqualFold(mailbox, "INBOX") {
		return "inbox"
	}
	for _, a := range attrs {
		switch a {
		case goimap.MailboxAttrSent:
			return "sentitems"
		case goimap.MailboxAttrDrafts:
			return "drafts"
		case goimap.MailboxAttrTrash:
			return "deleteditems"
		case goimap.MailboxAttrJunk:
			return "junkemail"
		case goimap.MailboxAttrArchive:
			return "archive"
		}
	}
	return ""
}

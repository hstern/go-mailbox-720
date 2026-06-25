package imap

import (
	"sort"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

// mapFlags classifies a message's IMAP flags into the Graph-mapped fields the
// envelope carries beyond \Seen (handled separately, since it predates this and
// drives IsRead): the \Flagged and \Draft system flags, and the user keywords
// that become Graph categories (per mail.IsUserKeyword — anything without a
// leading "\" or "$"). categories is sorted for a deterministic order and is nil
// when the message has no user keywords.
func mapFlags(flags []goimap.Flag) (flagged, draft bool, categories []string) {
	for _, f := range flags {
		switch f {
		case goimap.FlagFlagged:
			flagged = true
		case goimap.FlagDraft:
			draft = true
		default:
			if mail.IsUserKeyword(string(f)) {
				categories = append(categories, string(f))
			}
		}
	}
	sort.Strings(categories)
	return flagged, draft, categories
}

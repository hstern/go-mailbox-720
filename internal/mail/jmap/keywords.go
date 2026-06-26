package jmap

import (
	"sort"

	port "github.com/hstern/go-mailbox-720/internal/mail"
)

// keywordFlagged and keywordDraft are the RFC 8621 system keywords that map onto
// dedicated Graph message fields (flag.flagStatus and isDraft). keywordSeen (in
// jmap.go) is the third, driving IsRead.
const (
	keywordFlagged = "$flagged"
	keywordDraft   = "$draft"
)

// mapKeywords classifies an Email's keywords into the Graph-mapped fields beyond
// $seen (handled separately, driving IsRead): the $flagged and $draft system
// keywords, and the user keywords that become Graph categories (per
// port.IsUserKeyword — a keyword with no leading "$"). categories is sorted (a
// JMAP keywords map is unordered) and nil when the message has none.
func mapKeywords(keywords map[string]bool) (flagged, draft bool, categories []string) {
	for k, set := range keywords {
		if !set {
			continue
		}
		switch k {
		case keywordFlagged:
			flagged = true
		case keywordDraft:
			draft = true
		default:
			if port.IsUserKeyword(k) {
				categories = append(categories, k)
			}
		}
	}
	sort.Strings(categories)
	return flagged, draft, categories
}

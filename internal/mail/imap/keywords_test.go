package imap

import (
	"reflect"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func TestMapFlags(t *testing.T) {
	tests := []struct {
		name      string
		flags     []goimap.Flag
		wantFlag  bool
		wantDraft bool
		wantCats  []string
	}{
		{"none", nil, false, false, nil},
		{"seen is not a category", []goimap.Flag{goimap.FlagSeen}, false, false, nil},
		{"flagged", []goimap.Flag{goimap.FlagFlagged}, true, false, nil},
		{"draft", []goimap.Flag{goimap.FlagDraft}, false, true, nil},
		{"answered and deleted are dropped system flags", []goimap.Flag{goimap.FlagAnswered, goimap.FlagDeleted}, false, false, nil},
		{"dollar keywords are dropped", []goimap.Flag{"$Forwarded", "$MDNSent", "$Junk"}, false, false, nil},
		{"user keywords become sorted categories", []goimap.Flag{"Work", "Banking"}, false, false, []string{"Banking", "Work"}},
		{"mixed", []goimap.Flag{goimap.FlagSeen, goimap.FlagFlagged, goimap.FlagDraft, "Work", "$Phishing"}, true, true, []string{"Work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flagged, draft, cats := mapFlags(tc.flags)
			if flagged != tc.wantFlag || draft != tc.wantDraft {
				t.Errorf("mapFlags(%v) flagged/draft = %v/%v, want %v/%v", tc.flags, flagged, draft, tc.wantFlag, tc.wantDraft)
			}
			if !reflect.DeepEqual(cats, tc.wantCats) {
				t.Errorf("mapFlags(%v) categories = %v, want %v", tc.flags, cats, tc.wantCats)
			}
		})
	}
}

package jmap

import (
	"reflect"
	"testing"
)

func TestMapKeywords(t *testing.T) {
	tests := []struct {
		name      string
		keywords  map[string]bool
		wantFlag  bool
		wantDraft bool
		wantCats  []string
	}{
		{"none", nil, false, false, nil},
		{"seen is not a category", map[string]bool{keywordSeen: true}, false, false, nil},
		{"flagged", map[string]bool{keywordFlagged: true}, true, false, nil},
		{"draft", map[string]bool{keywordDraft: true}, false, true, nil},
		{"false-valued keyword is ignored", map[string]bool{keywordFlagged: false, "Work": false}, false, false, nil},
		{"dollar keywords are dropped", map[string]bool{"$forwarded": true, "$junk": true}, false, false, nil},
		{"user keywords become sorted categories", map[string]bool{"Work": true, "Banking": true}, false, false, []string{"Banking", "Work"}},
		{"mixed", map[string]bool{keywordSeen: true, keywordFlagged: true, keywordDraft: true, "Work": true, "$phishing": true}, true, true, []string{"Work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flagged, draft, cats := mapKeywords(tc.keywords)
			if flagged != tc.wantFlag || draft != tc.wantDraft {
				t.Errorf("mapKeywords(%v) flagged/draft = %v/%v, want %v/%v", tc.keywords, flagged, draft, tc.wantFlag, tc.wantDraft)
			}
			if !reflect.DeepEqual(cats, tc.wantCats) {
				t.Errorf("mapKeywords(%v) categories = %v, want %v", tc.keywords, cats, tc.wantCats)
			}
		})
	}
}

package jmap

import (
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/hstern/go-mailbox-720/internal/odata"
)

// parseFilter is a test helper that parses an OData $filter string into the
// neutral AST, failing the test on a parse error.
func parseFilter(t *testing.T, s string) *odata.Filter {
	t.Helper()
	f, err := odata.Parse(s)
	if err != nil {
		t.Fatalf("odata.Parse(%q): %v", s, err)
	}
	return f
}

func TestTranslateFilterCombinesWithMailbox(t *testing.T) {
	base := &email.FilterCondition{InMailbox: "mbx"}
	f := parseFilter(t, "subject eq 'hello'")
	got := translateFilter(f.Root, base)

	op, ok := got.(*email.FilterOperator)
	if !ok {
		t.Fatalf("got %T, want *email.FilterOperator", got)
	}
	if op.Operator != gojmap.OperatorAND {
		t.Errorf("operator = %q, want AND", op.Operator)
	}
	if len(op.Conditions) != 2 {
		t.Fatalf("conditions = %d, want 2 (mailbox + subject)", len(op.Conditions))
	}
	if op.Conditions[0] != email.Filter(base) {
		t.Error("first condition should be the mailbox scope")
	}
	sub, ok := op.Conditions[1].(*email.FilterCondition)
	if !ok || sub.Subject != "hello" {
		t.Errorf("second condition = %+v, want subject 'hello'", op.Conditions[1])
	}
}

func TestTranslateFilterNoPredicateKeepsMailboxOnly(t *testing.T) {
	base := &email.FilterCondition{InMailbox: "mbx"}
	// endswith has no JMAP equivalent and is dropped → just the mailbox scope.
	f := parseFilter(t, "endswith(subject,'x')")
	got := translateFilter(f.Root, base)
	if got != email.Filter(base) {
		t.Errorf("got %T, want the base mailbox condition unchanged", got)
	}
}

func TestTranslateComparisonFields(t *testing.T) {
	tests := []struct {
		name   string
		filter string
		check  func(*testing.T, *email.FilterCondition)
	}{
		{"subject", "subject eq 'hi'", func(t *testing.T, fc *email.FilterCondition) {
			if fc.Subject != "hi" {
				t.Errorf("Subject = %q, want hi", fc.Subject)
			}
		}},
		{"from", "from/emailAddress/address eq 'a@b.com'", func(t *testing.T, fc *email.FilterCondition) {
			if fc.From != "a@b.com" {
				t.Errorf("From = %q, want a@b.com", fc.From)
			}
		}},
		{"isRead true", "isRead eq true", func(t *testing.T, fc *email.FilterCondition) {
			if fc.HasKeyword != keywordSeen {
				t.Errorf("HasKeyword = %q, want %q", fc.HasKeyword, keywordSeen)
			}
		}},
		{"isRead false", "isRead eq false", func(t *testing.T, fc *email.FilterCondition) {
			if fc.NotKeyword != keywordSeen {
				t.Errorf("NotKeyword = %q, want %q", fc.NotKeyword, keywordSeen)
			}
		}},
		{"hasAttachments", "hasAttachments eq true", func(t *testing.T, fc *email.FilterCondition) {
			if !fc.HasAttachment {
				t.Error("HasAttachment = false, want true")
			}
		}},
		{"receivedDateTime ge", "receivedDateTime ge 2025-01-02T03:04:05Z", func(t *testing.T, fc *email.FilterCondition) {
			if fc.After == nil {
				t.Fatal("After = nil, want a time")
			}
			if got := fc.After.UTC().Format("2006-01-02"); got != "2025-01-02" {
				t.Errorf("After date = %q, want 2025-01-02", got)
			}
		}},
		{"contains", "contains(subject,'urgent')", func(t *testing.T, fc *email.FilterCondition) {
			if fc.Subject != "urgent" {
				t.Errorf("Subject = %q, want urgent", fc.Subject)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := parseFilter(t, tc.filter)
			got := translateNode(f.Root)
			fc, ok := got.(*email.FilterCondition)
			if !ok {
				t.Fatalf("got %T, want *email.FilterCondition", got)
			}
			tc.check(t, fc)
		})
	}
}

func TestTranslateLogical(t *testing.T) {
	f := parseFilter(t, "subject eq 'a' and isRead eq true")
	got := translateNode(f.Root)
	op, ok := got.(*email.FilterOperator)
	if !ok {
		t.Fatalf("got %T, want *email.FilterOperator", got)
	}
	if op.Operator != gojmap.OperatorAND || len(op.Conditions) != 2 {
		t.Errorf("got operator %q with %d conditions, want AND with 2", op.Operator, len(op.Conditions))
	}
}

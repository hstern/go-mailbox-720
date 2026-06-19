package odata

import (
	"errors"
	"testing"
)

// mailboxFields is the allow-list a message-collection handler might use.
var mailboxFields = []string{
	"subject",
	"isRead",
	"receivedDateTime",
	"from/emailAddress/address",
}

func TestParseSimpleComparisons(t *testing.T) {
	tests := []struct {
		name      string
		filter    string
		wantField string
		wantOp    CompareOp
		wantKind  LiteralKind
		wantValue string
	}{
		{"string eq", "subject eq 'hi'", "subject", OpEq, StringLiteral, "hi"},
		{"string ne", "subject ne 'bye'", "subject", OpNe, StringLiteral, "bye"},
		{"bool eq", "isRead eq true", "isRead", OpEq, BooleanLiteral, "true"},
		{"datetime gt", "receivedDateTime gt 2020-01-01T00:00:00Z", "receivedDateTime", OpGt, DateTimeLiteral, "2020-01-01T00:00:00Z"},
		{"nested field", "from/emailAddress/address eq 'a@b.com'", "from/emailAddress/address", OpEq, StringLiteral, "a@b.com"},
		{"escaped quote", "subject eq 'O''Brien'", "subject", OpEq, StringLiteral, "O'Brien"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse(tt.filter)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.filter, err)
			}
			cmp, ok := f.Root.(*Comparison)
			if !ok {
				t.Fatalf("root = %T, want *Comparison", f.Root)
			}
			if cmp.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", cmp.Field, tt.wantField)
			}
			if cmp.Op != tt.wantOp {
				t.Errorf("Op = %q, want %q", cmp.Op, tt.wantOp)
			}
			if cmp.Value.Kind != tt.wantKind {
				t.Errorf("Value.Kind = %d, want %d", cmp.Value.Kind, tt.wantKind)
			}
			if cmp.Value.Value != tt.wantValue {
				t.Errorf("Value.Value = %q, want %q", cmp.Value.Value, tt.wantValue)
			}
		})
	}
}

func TestParseLogicalNesting(t *testing.T) {
	t.Run("and", func(t *testing.T) {
		f, err := Parse("subject eq 'hi' and isRead eq true")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		root, ok := f.Root.(*Logical)
		if !ok {
			t.Fatalf("root = %T, want *Logical", f.Root)
		}
		if root.Op != OpAnd {
			t.Errorf("Op = %q, want and", root.Op)
		}
		if len(root.Operands) != 2 {
			t.Fatalf("len(Operands) = %d, want 2", len(root.Operands))
		}
		for i, o := range root.Operands {
			if _, ok := o.(*Comparison); !ok {
				t.Errorf("operand %d = %T, want *Comparison", i, o)
			}
		}
	})

	t.Run("or", func(t *testing.T) {
		f, err := Parse("subject eq 'a' or subject eq 'b'")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		root, ok := f.Root.(*Logical)
		if !ok || root.Op != OpOr {
			t.Fatalf("root = %+v, want *Logical or", f.Root)
		}
	})

	t.Run("not", func(t *testing.T) {
		f, err := Parse("not (isRead eq true)")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		root, ok := f.Root.(*Logical)
		if !ok || root.Op != OpNot {
			t.Fatalf("root = %+v, want *Logical not", f.Root)
		}
		if len(root.Operands) != 1 {
			t.Fatalf("len(Operands) = %d, want 1", len(root.Operands))
		}
	})

	t.Run("deep nesting", func(t *testing.T) {
		f, err := Parse("(subject eq 'a' or subject eq 'b') and not (isRead eq false)")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		root, ok := f.Root.(*Logical)
		if !ok || root.Op != OpAnd {
			t.Fatalf("root = %+v, want *Logical and", f.Root)
		}
		left, ok := root.Operands[0].(*Logical)
		if !ok || left.Op != OpOr {
			t.Errorf("left = %+v, want *Logical or", root.Operands[0])
		}
		right, ok := root.Operands[1].(*Logical)
		if !ok || right.Op != OpNot {
			t.Errorf("right = %+v, want *Logical not", root.Operands[1])
		}
	})
}

func TestParseFunctions(t *testing.T) {
	tests := []struct {
		name      string
		filter    string
		wantName  FuncName
		wantField string
		wantArg   string
	}{
		{"startswith", "startswith(subject,'foo')", FuncStartsWith, "subject", "foo"},
		{"endswith", "endswith(subject,'bar')", FuncEndsWith, "subject", "bar"},
		{"contains", "contains(subject,'baz')", FuncContains, "subject", "baz"},
		{"contains nested field", "contains(from/emailAddress/address,'@b.com')", FuncContains, "from/emailAddress/address", "@b.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse(tt.filter)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.filter, err)
			}
			fn, ok := f.Root.(*Function)
			if !ok {
				t.Fatalf("root = %T, want *Function", f.Root)
			}
			if fn.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", fn.Name, tt.wantName)
			}
			if fn.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", fn.Field, tt.wantField)
			}
			if fn.Arg != tt.wantArg {
				t.Errorf("Arg = %q, want %q", fn.Arg, tt.wantArg)
			}
		})
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, filter := range []string{
		"",
		"   ",
		"subject eq",          // missing right operand
		"subject eq 'hi' and", // dangling connective
		"eq 'hi'",             // no field
		"((subject eq 'hi'",   // unbalanced parens
		"subject 'hi'",        // missing operator
	} {
		t.Run(filter, func(t *testing.T) {
			if _, err := Parse(filter); err == nil {
				t.Errorf("Parse(%q) = nil error, want error", filter)
			} else if !errors.Is(err, ErrMalformedFilter) {
				t.Errorf("Parse(%q) error = %v, want ErrMalformedFilter", filter, err)
			}
		})
	}
}

func TestParseRejectsUnsupportedOperators(t *testing.T) {
	tests := []struct {
		filter string
		want   error
	}{
		{"subject has 'hi'", ErrUnsupportedOperator},
		{"subject in ('a','b')", ErrUnsupportedOperator},
		{"price add 1 eq 2", ErrUnsupportedOperator},
		{"substring(subject,1) eq 'x'", ErrUnsupportedFunction},
		{"length(subject) eq 5", ErrUnsupportedFunction},
		{"tolower(subject) eq 'x'", ErrUnsupportedFunction},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			_, err := Parse(tt.filter)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want %v", tt.filter, tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("Parse(%q) error = %v, want %v", tt.filter, err, tt.want)
			}
		})
	}
}

func TestValidateAllowList(t *testing.T) {
	tests := []struct {
		name    string
		filter  string
		wantErr bool
	}{
		{"allowed simple", "subject eq 'hi'", false},
		{"allowed nested", "from/emailAddress/address eq 'a@b.com'", false},
		{"allowed function", "startswith(subject,'foo')", false},
		{"allowed compound", "subject eq 'hi' and isRead eq true", false},
		{"unknown field", "body eq 'hi'", true},
		{"unknown nested field", "to/emailAddress/address eq 'a@b.com'", true},
		{"unknown in compound", "subject eq 'hi' or sender eq 'x'", true},
		{"unknown in function", "contains(body,'x')", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse(tt.filter)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.filter, err)
			}
			err = f.Validate(mailboxFields)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate(%q) = nil error, want ErrUnknownField", tt.filter)
				} else if !errors.Is(err, ErrUnknownField) {
					t.Errorf("Validate(%q) error = %v, want ErrUnknownField", tt.filter, err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tt.filter, err)
			}
		})
	}
}

func TestValidateEmptyAllowListRejectsAnyField(t *testing.T) {
	f, err := Parse("subject eq 'hi'")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := f.Validate(nil); !errors.Is(err, ErrUnknownField) {
		t.Errorf("Validate(nil) = %v, want ErrUnknownField", err)
	}
}

func TestFields(t *testing.T) {
	f, err := Parse("subject eq 'hi' and (isRead eq true or startswith(subject,'x'))")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := f.Fields()
	want := []string{"subject", "isRead"} // subject is de-duplicated
	if len(got) != len(want) {
		t.Fatalf("Fields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Fields()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

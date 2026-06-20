package jmap

import (
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/hstern/go-mailbox-720/internal/odata"
)

// This file translates the neutral odata.Filter AST into a JMAP Email filter
// (RFC 8621 §4.4.1). JMAP is a far closer match to the Graph $filter surface
// than IMAP SEARCH: its FilterCondition already carries subject/from/to/cc/bcc
// substring predicates, a hasAttachment flag, keyword (read-state) conditions,
// and before/after date bounds, and its FilterOperator gives AND/OR/NOT
// directly. So translation is near pass-through.
//
// JMAP evaluates the filter server-side. Unlike the IMAP adapter there is no
// client-side re-check: the server is the source of truth for what the filter
// selects. Predicates the translation cannot express (an unmapped field, or a
// comparison shape JMAP has no condition for) are dropped to a broader match —
// the same best-effort posture the IMAP adapter takes, where a $filter narrows
// but the result is always re-paged. A dropped predicate widens, never narrows
// incorrectly, because an untranslatable leaf becomes the zero FilterCondition
// (matches everything) rather than excluding rows.
//
// The translated filter is AND-combined with the mailbox scope so a folder
// listing's $filter still stays within its folder.

// translateFilter combines the mailbox scope (base) with the translation of the
// $filter AST under AND. A nil/empty translation leaves just the mailbox scope.
func translateFilter(n odata.Node, base *email.FilterCondition) email.Filter {
	f := translateNode(n)
	if f == nil {
		return base
	}
	return &email.FilterOperator{
		Operator:   gojmap.OperatorAND,
		Conditions: []email.Filter{base, f},
	}
}

// translateNode maps one AST node to a JMAP Email filter, or nil when nothing
// useful can be expressed (the caller treats nil as "no additional constraint").
func translateNode(n odata.Node) email.Filter {
	switch t := n.(type) {
	case *odata.Logical:
		return translateLogical(t)
	case *odata.Comparison:
		return translateComparison(t)
	case *odata.Function:
		return translateFunction(t)
	}
	return nil
}

func translateLogical(l *odata.Logical) email.Filter {
	op := map[odata.LogicalOp]gojmap.Operator{
		odata.OpAnd: gojmap.OperatorAND,
		odata.OpOr:  gojmap.OperatorOR,
		odata.OpNot: gojmap.OperatorNOT,
	}[l.Op]
	conds := make([]email.Filter, 0, len(l.Operands))
	for _, o := range l.Operands {
		if c := translateNode(o); c != nil {
			conds = append(conds, c)
		}
	}
	if len(conds) == 0 {
		return nil
	}
	return &email.FilterOperator{Operator: op, Conditions: conds}
}

// translateComparison maps a field-to-literal comparison. String equality on a
// header field becomes the matching substring condition (JMAP has no exact
// header equality in the base condition set, so this widens to substring — re-
// paging keeps the result bounded); isRead maps to a keyword condition; date
// comparisons map to before/after; hasAttachments maps to the flag.
func translateComparison(c *odata.Comparison) email.Filter {
	switch c.Field {
	case "subject":
		return strCond(func(fc *email.FilterCondition) { fc.Subject = c.Value.Value }, c)
	case "from", "from/emailAddress/address":
		return strCond(func(fc *email.FilterCondition) { fc.From = c.Value.Value }, c)
	case "to", "to/emailAddress/address":
		return strCond(func(fc *email.FilterCondition) { fc.To = c.Value.Value }, c)
	case "isRead":
		return readCond(c)
	case "hasAttachments":
		if c.Op == odata.OpEq && c.Value.Kind == odata.BooleanLiteral && c.Value.Value == "true" {
			return &email.FilterCondition{HasAttachment: true}
		}
		return nil
	case "receivedDateTime":
		return dateCond(c)
	}
	return nil
}

// strCond applies set (a substring predicate setter) only for an equality on a
// string literal; other operators on a string field have no JMAP equivalent and
// are dropped (widening).
func strCond(set func(*email.FilterCondition), c *odata.Comparison) email.Filter {
	if c.Op != odata.OpEq || c.Value.Kind != odata.StringLiteral {
		return nil
	}
	fc := &email.FilterCondition{}
	set(fc)
	return fc
}

// readCond maps isRead eq true/false to a $seen keyword presence/absence
// condition.
func readCond(c *odata.Comparison) email.Filter {
	if c.Op != odata.OpEq || c.Value.Kind != odata.BooleanLiteral {
		return nil
	}
	if c.Value.Value == "true" {
		return &email.FilterCondition{HasKeyword: keywordSeen}
	}
	return &email.FilterCondition{NotKeyword: keywordSeen}
}

// dateCond maps a receivedDateTime comparison to before/after bounds. ge/gt map
// to After (inclusive on the wire; JMAP's after is "received at or after"); le/
// lt map to Before. Other operators are dropped.
func dateCond(c *odata.Comparison) email.Filter {
	if c.Value.Kind != odata.DateTimeLiteral {
		return nil
	}
	t, err := time.Parse(time.RFC3339, c.Value.Value)
	if err != nil {
		return nil
	}
	switch c.Op {
	case odata.OpGe, odata.OpGt:
		return &email.FilterCondition{After: &t}
	case odata.OpLe, odata.OpLt:
		return &email.FilterCondition{Before: &t}
	}
	return nil
}

// translateFunction maps contains/startswith on a text field to the matching
// JMAP substring condition. JMAP substring matching is "contains" semantics, so
// startswith widens to contains (re-paging keeps the result bounded). endswith
// has no JMAP equivalent and is dropped.
func translateFunction(f *odata.Function) email.Filter {
	if f.Name != odata.FuncContains && f.Name != odata.FuncStartsWith {
		return nil
	}
	switch f.Field {
	case "subject":
		return &email.FilterCondition{Subject: f.Arg}
	case "from", "from/emailAddress/address":
		return &email.FilterCondition{From: f.Arg}
	case "to", "to/emailAddress/address":
		return &email.FilterCondition{To: f.Arg}
	}
	return nil
}

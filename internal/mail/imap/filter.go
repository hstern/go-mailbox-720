package imap

import (
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// This file translates a neutral odata.Filter AST into the practical subset of
// IMAP SEARCH the server can express, and provides a client-side evaluator that
// re-checks the full AST against a mapped mail.Message.
//
// The split is deliberate. IMAP SEARCH is a coarse, best-effort narrowing pass:
// translateFilter never returns criteria that match *fewer* messages than the
// filter intends, so the SEARCH result is always a superset of the true match
// set. evalFilter then makes the result exact. A predicate IMAP cannot express
// (for example endswith on a header, or hasAttachments) simply contributes no
// SEARCH criteria — it is enforced entirely client-side by evalFilter. This way
// correctness never depends on full IMAP SEARCH support.
//
// Supported mappings (everything else falls through to client-side only):
//
//	contains/startswith(subject,'x')        -> SearchCriteria.Header "Subject"
//	contains/startswith(from...,'x')        -> SearchCriteria.Header "From"
//	contains/startswith(to...,'x')          -> SearchCriteria.Header "To"
//	subject/from/to eq|ne 'x'               -> Header (ne also negates client-side)
//	receivedDateTime ge|gt <datetime>       -> SearchCriteria.Since
//	receivedDateTime le|lt <datetime>       -> SearchCriteria.Before
//	isRead eq true|false                    -> Seen / Unseen flag
//	and                                     -> intersect (criteria.And)
//	or                                      -> SearchCriteria.Or
//	not                                     -> SearchCriteria.Not

// Recognized field paths. Address fields accept both the bare name and the Graph
// nested navigation path.
const (
	fieldSubject          = "subject"
	fieldFrom             = "from"
	fieldFromAddress      = "from/emailAddress/address"
	fieldTo               = "to"
	fieldToAddress        = "to/emailAddress/address"
	fieldReceivedDateTime = "receivedDateTime"
	fieldIsRead           = "isRead"
	fieldHasAttachments   = "hasAttachments"
)

// translateFilter converts a filter AST into IMAP SEARCH criteria. The returned
// criteria are a superset filter: they never exclude a message the AST would
// keep, so the SEARCH result must still be refined by evalFilter. A node that
// has no IMAP equivalent yields the empty (match-all) criteria.
func translateFilter(n odata.Node) *goimap.SearchCriteria {
	c := &goimap.SearchCriteria{}
	switch t := n.(type) {
	case *odata.Logical:
		switch t.Op {
		case odata.OpAnd:
			// Intersection: stack both operands' criteria together.
			left := translateFilter(t.Operands[0])
			right := translateFilter(t.Operands[1])
			c.And(left)
			c.And(right)
		case odata.OpOr:
			left := translateFilter(t.Operands[0])
			right := translateFilter(t.Operands[1])
			c.Or = append(c.Or, [2]goimap.SearchCriteria{*left, *right})
		case odata.OpNot:
			// Negation is only safe to push into SEARCH when the inner predicate
			// translates exactly; otherwise leave it match-all and let evalFilter
			// handle it (a too-broad SEARCH is still correct after refinement).
			if inner := translateExact(t.Operands[0]); inner != nil {
				c.Not = append(c.Not, *inner)
			}
		}
	case *odata.Comparison:
		if mapped := translateComparison(t); mapped != nil {
			return mapped
		}
	case *odata.Function:
		if mapped := translateFunction(t); mapped != nil {
			return mapped
		}
	}
	return c
}

// translateExact returns criteria only when the node maps onto IMAP SEARCH
// exactly (no client-side refinement needed), so it is safe under NOT. It
// returns nil otherwise.
func translateExact(n odata.Node) *goimap.SearchCriteria {
	switch t := n.(type) {
	case *odata.Comparison:
		// Flag and header-equality comparisons translate exactly.
		switch t.Field {
		case fieldIsRead, fieldSubject, fieldFrom, fieldFromAddress, fieldTo, fieldToAddress:
			if t.Op == odata.OpEq {
				return translateComparison(t)
			}
		}
	case *odata.Function:
		// Substring SEARCH is itself a superset (case-insensitive, no anchor), so
		// it is not exact and must not be negated at the SEARCH layer.
	}
	return nil
}

// translateComparison maps a single comparison onto SEARCH criteria, or nil when
// IMAP cannot express it.
func translateComparison(c *odata.Comparison) *goimap.SearchCriteria {
	switch c.Field {
	case fieldSubject:
		return headerCriteria("Subject", c)
	case fieldFrom, fieldFromAddress:
		return headerCriteria("From", c)
	case fieldTo, fieldToAddress:
		return headerCriteria("To", c)
	case fieldReceivedDateTime:
		return dateCriteria(c)
	case fieldIsRead:
		return flagCriteria(c)
	}
	return nil
}

// headerCriteria handles eq/ne on a textual header. eq becomes a header substring
// match (a superset, refined client-side); ne yields match-all here and is left
// to evalFilter.
func headerCriteria(key string, c *odata.Comparison) *goimap.SearchCriteria {
	if c.Op != odata.OpEq || c.Value.Kind != odata.StringLiteral {
		return nil
	}
	return &goimap.SearchCriteria{
		Header: []goimap.SearchCriteriaHeaderField{{Key: key, Value: c.Value.Value}},
	}
}

// dateCriteria maps receivedDateTime ordering onto Since/Before. IMAP SEARCH
// SINCE/BEFORE compare on date only (time is ignored), so this is a superset that
// evalFilter then tightens to the exact instant.
func dateCriteria(c *odata.Comparison) *goimap.SearchCriteria {
	if c.Value.Kind != odata.DateTimeLiteral {
		return nil
	}
	ts, ok := parseDateTime(c.Value.Value)
	if !ok {
		return nil
	}
	out := &goimap.SearchCriteria{}
	switch c.Op {
	case odata.OpGe, odata.OpGt:
		out.Since = ts
	case odata.OpLe, odata.OpLt:
		out.Before = ts
	default:
		return nil
	}
	return out
}

// flagCriteria maps isRead eq true/false onto the Seen/Unseen flag.
func flagCriteria(c *odata.Comparison) *goimap.SearchCriteria {
	if c.Op != odata.OpEq || c.Value.Kind != odata.BooleanLiteral {
		return nil
	}
	out := &goimap.SearchCriteria{}
	if c.Value.Value == "true" {
		out.Flag = []goimap.Flag{goimap.FlagSeen}
	} else {
		out.NotFlag = []goimap.Flag{goimap.FlagSeen}
	}
	return out
}

// translateFunction maps contains/startswith on a textual field onto a header
// substring SEARCH (a superset, refined client-side). endswith has no IMAP
// equivalent and returns nil (enforced entirely by evalFilter).
func translateFunction(f *odata.Function) *goimap.SearchCriteria {
	if f.Name != odata.FuncContains && f.Name != odata.FuncStartsWith {
		return nil
	}
	var key string
	switch f.Field {
	case fieldSubject:
		key = "Subject"
	case fieldFrom, fieldFromAddress:
		key = "From"
	case fieldTo, fieldToAddress:
		key = "To"
	default:
		return nil
	}
	return &goimap.SearchCriteria{
		Header: []goimap.SearchCriteriaHeaderField{{Key: key, Value: f.Arg}},
	}
}

// evalFilter evaluates the full filter AST against a mapped mail.Message. This is
// the authoritative pass: it makes the coarse IMAP SEARCH result exact and
// enforces any predicate SEARCH could not express. An unrecognized node matches
// (true) so a partially-understood filter never silently drops messages.
func evalFilter(n odata.Node, m mail.Message) bool {
	switch t := n.(type) {
	case *odata.Logical:
		switch t.Op {
		case odata.OpAnd:
			return evalFilter(t.Operands[0], m) && evalFilter(t.Operands[1], m)
		case odata.OpOr:
			return evalFilter(t.Operands[0], m) || evalFilter(t.Operands[1], m)
		case odata.OpNot:
			return !evalFilter(t.Operands[0], m)
		}
	case *odata.Comparison:
		return evalComparison(t, m)
	case *odata.Function:
		return evalFunction(t, m)
	}
	return true
}

func evalComparison(c *odata.Comparison, m mail.Message) bool {
	switch c.Field {
	case fieldSubject:
		return compareString(c.Op, m.Subject, c.Value.Value)
	case fieldFrom, fieldFromAddress:
		return compareString(c.Op, m.From.Email, c.Value.Value)
	case fieldTo, fieldToAddress:
		return anyAddress(c.Op, m.To, c.Value.Value)
	case fieldReceivedDateTime:
		return compareDate(c.Op, m.ReceivedAt, c.Value.Value)
	case fieldIsRead:
		return compareBool(c.Op, m.IsRead, c.Value.Value)
	case fieldHasAttachments:
		return compareBool(c.Op, m.HasAttachments, c.Value.Value)
	}
	return true
}

func evalFunction(f *odata.Function, m mail.Message) bool {
	var field string
	switch f.Field {
	case fieldSubject:
		field = m.Subject
	case fieldFrom, fieldFromAddress:
		field = m.From.Email
	case fieldTo, fieldToAddress:
		// Match if the predicate holds for any recipient.
		for _, a := range m.To {
			if applyStringFunc(f.Name, a.Email, f.Arg) {
				return true
			}
		}
		return false
	default:
		return true
	}
	return applyStringFunc(f.Name, field, f.Arg)
}

// applyStringFunc evaluates a string predicate function case-insensitively, to
// match Graph's case-insensitive string filtering and IMAP SEARCH semantics.
func applyStringFunc(name odata.FuncName, field, arg string) bool {
	f := strings.ToLower(field)
	a := strings.ToLower(arg)
	switch name {
	case odata.FuncContains:
		return strings.Contains(f, a)
	case odata.FuncStartsWith:
		return strings.HasPrefix(f, a)
	case odata.FuncEndsWith:
		return strings.HasSuffix(f, a)
	}
	return false
}

// compareString applies eq/ne to a string field, case-insensitively.
func compareString(op odata.CompareOp, field, want string) bool {
	eq := strings.EqualFold(field, want)
	switch op {
	case odata.OpEq:
		return eq
	case odata.OpNe:
		return !eq
	}
	return false
}

// anyAddress is eq/ne over a list of recipients: eq matches if any recipient
// equals want; ne matches if none does.
func anyAddress(op odata.CompareOp, addrs []mail.Address, want string) bool {
	var found bool
	for _, a := range addrs {
		if strings.EqualFold(a.Email, want) {
			found = true
			break
		}
	}
	switch op {
	case odata.OpEq:
		return found
	case odata.OpNe:
		return !found
	}
	return false
}

// compareBool applies eq/ne to a boolean field.
func compareBool(op odata.CompareOp, field bool, want string) bool {
	w := want == "true"
	switch op {
	case odata.OpEq:
		return field == w
	case odata.OpNe:
		return field != w
	}
	return false
}

// compareDate applies an ordering/equality operator to receivedDateTime.
func compareDate(op odata.CompareOp, field time.Time, want string) bool {
	ts, ok := parseDateTime(want)
	if !ok {
		return true // unparseable bound: do not drop the message
	}
	switch op {
	case odata.OpEq:
		return field.Equal(ts)
	case odata.OpNe:
		return !field.Equal(ts)
	case odata.OpGt:
		return field.After(ts)
	case odata.OpGe:
		return field.After(ts) || field.Equal(ts)
	case odata.OpLt:
		return field.Before(ts)
	case odata.OpLe:
		return field.Before(ts) || field.Equal(ts)
	}
	return false
}

// parseDateTime parses an OData datetime literal: an RFC3339 timestamp (with
// optional fractional seconds) or a bare date (YYYY-MM-DD, treated as midnight
// UTC). It reports ok=false for anything it cannot read.
func parseDateTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}

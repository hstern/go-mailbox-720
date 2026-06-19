// Package odata parses and validates the OData v4 $filter query option that
// Microsoft Graph clients send on collection requests (for example
// $filter=subject eq 'hi'). It produces a small, provider-agnostic filter AST
// and an allow-list validator so a mailbox handler can reject a filter that
// references unknown fields, operators, or functions before it ever reaches a
// backend.
//
// Parsing leans on github.com/CiscoM31/godata for the tokenizer and grammar;
// this package converts godata's generic parse tree into its own minimal AST
// (Comparison, Logical, Function) so callers never depend on godata's types.
//
// Scope: parse and validate only. Translating a *Filter into a concrete backend
// query (IMAP SEARCH, JMAP, ...) and executing it is a separate concern handled
// elsewhere (MB720-6). A successful Parse means the filter is well-formed and
// uses the supported subset; a successful Validate means every referenced field
// is permitted. Callers map the returned errors (all wrapping the sentinels
// below) to a Graph 400 response.
package odata

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CiscoM31/godata"
)

// Sentinel errors returned (wrapped) by Parse and Validate. Callers use
// errors.Is to classify a failure; every one maps to an HTTP 400.
var (
	// ErrMalformedFilter is returned when the input is not a syntactically
	// valid $filter expression, or its shape is not one this package models
	// (for example a comparison whose left side is not a field).
	ErrMalformedFilter = errors.New("odata: malformed $filter")
	// ErrUnsupportedOperator is returned for a comparison or logical operator
	// outside the supported set (eq, ne, gt, ge, lt, le, and, or, not).
	ErrUnsupportedOperator = errors.New("odata: unsupported operator")
	// ErrUnsupportedFunction is returned for a function outside the supported
	// set (startswith, endswith, contains).
	ErrUnsupportedFunction = errors.New("odata: unsupported function")
	// ErrUnknownField is returned by Validate when the filter references a
	// field that is not in the caller's allow-list.
	ErrUnknownField = errors.New("odata: unknown field")
)

// CompareOp is a binary comparison operator.
type CompareOp string

// The supported comparison operators.
const (
	OpEq CompareOp = "eq"
	OpNe CompareOp = "ne"
	OpGt CompareOp = "gt"
	OpGe CompareOp = "ge"
	OpLt CompareOp = "lt"
	OpLe CompareOp = "le"
)

// LogicalOp is a boolean connective. OpNot is unary; OpAnd and OpOr are binary.
type LogicalOp string

// The supported logical operators.
const (
	OpAnd LogicalOp = "and"
	OpOr  LogicalOp = "or"
	OpNot LogicalOp = "not"
)

// FuncName is a supported string predicate function.
type FuncName string

// The supported filter functions.
const (
	FuncStartsWith FuncName = "startswith"
	FuncEndsWith   FuncName = "endswith"
	FuncContains   FuncName = "contains"
)

// LiteralKind classifies the type of a literal value in a comparison.
type LiteralKind int

// The literal kinds this package distinguishes.
const (
	StringLiteral LiteralKind = iota
	NumberLiteral
	BooleanLiteral
	DateTimeLiteral
	NullLiteral
)

// Literal is a constant operand in a comparison. Value holds the normalized
// text: for a StringLiteral the surrounding single quotes are stripped and
// doubled quotes (”) are unescaped; for every other kind it is the literal
// token as written.
type Literal struct {
	Kind  LiteralKind
	Value string
}

// Node is a single node in the filter AST. The concrete types are *Comparison,
// *Logical, and *Function.
type Node interface {
	isNode()
}

// Comparison is a field-to-literal comparison, e.g. subject eq 'hi'. Field is
// the (possibly nested, slash-joined) property path on the left, e.g.
// "from/emailAddress/address".
type Comparison struct {
	Field string
	Op    CompareOp
	Value Literal
}

func (*Comparison) isNode() {}

// Logical is a boolean combination of one or two operands. For OpNot there is a
// single operand; for OpAnd and OpOr there are two.
type Logical struct {
	Op       LogicalOp
	Operands []Node
}

func (*Logical) isNode() {}

// Function is a string predicate over a field, e.g. startswith(subject,'foo').
// Arg is the unquoted string argument.
type Function struct {
	Name  FuncName
	Field string
	Arg   string
}

func (*Function) isNode() {}

// Filter is a parsed $filter expression.
type Filter struct {
	Root Node
}

// Parse parses an OData $filter string into a Filter. It returns an error
// wrapping ErrMalformedFilter for syntactically invalid or unmodelable input,
// ErrUnsupportedOperator for an operator outside the supported set, or
// ErrUnsupportedFunction for an unsupported function. Parse does not check
// field names; use Filter.Validate for that.
func Parse(filter string) (*Filter, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, fmt.Errorf("%w: empty filter", ErrMalformedFilter)
	}
	q, err := godata.ParseFilterString(context.Background(), filter)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedFilter, err)
	}
	if q == nil || q.Tree == nil {
		return nil, fmt.Errorf("%w: empty parse tree", ErrMalformedFilter)
	}
	root, err := convert(q.Tree)
	if err != nil {
		return nil, err
	}
	return &Filter{Root: root}, nil
}

// convert turns a godata parse node into a Filter AST node.
func convert(n *godata.ParseNode) (Node, error) {
	if n == nil || n.Token == nil {
		return nil, fmt.Errorf("%w: empty node", ErrMalformedFilter)
	}
	switch v := n.Token.Value; v {
	case "and", "or":
		return convertBinaryLogical(n, LogicalOp(v))
	case "not":
		return convertNot(n)
	case "eq", "ne", "gt", "ge", "lt", "le":
		return convertComparison(n, CompareOp(v))
	case "startswith", "endswith", "contains":
		return convertFunction(n, FuncName(v))
	default:
		// Anything else is outside the supported subset. Distinguish a
		// function call from an operator so the caller gets a precise error.
		if n.Token.Type == godata.ExpressionTokenFunc {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedFunction, v)
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedOperator, v)
	}
}

func convertBinaryLogical(n *godata.ParseNode, op LogicalOp) (Node, error) {
	if len(n.Children) != 2 {
		return nil, fmt.Errorf("%w: %s expects 2 operands, got %d", ErrMalformedFilter, op, len(n.Children))
	}
	left, err := convert(n.Children[0])
	if err != nil {
		return nil, err
	}
	right, err := convert(n.Children[1])
	if err != nil {
		return nil, err
	}
	return &Logical{Op: op, Operands: []Node{left, right}}, nil
}

func convertNot(n *godata.ParseNode) (Node, error) {
	if len(n.Children) != 1 {
		return nil, fmt.Errorf("%w: not expects 1 operand, got %d", ErrMalformedFilter, len(n.Children))
	}
	operand, err := convert(n.Children[0])
	if err != nil {
		return nil, err
	}
	return &Logical{Op: OpNot, Operands: []Node{operand}}, nil
}

func convertComparison(n *godata.ParseNode, op CompareOp) (Node, error) {
	if len(n.Children) != 2 {
		return nil, fmt.Errorf("%w: %s expects 2 operands, got %d", ErrMalformedFilter, op, len(n.Children))
	}
	field, err := fieldPath(n.Children[0])
	if err != nil {
		return nil, err
	}
	value, err := literalFromNode(n.Children[1])
	if err != nil {
		return nil, err
	}
	return &Comparison{Field: field, Op: op, Value: value}, nil
}

func convertFunction(n *godata.ParseNode, name FuncName) (Node, error) {
	if len(n.Children) != 2 {
		return nil, fmt.Errorf("%w: %s expects 2 arguments, got %d", ErrMalformedFilter, name, len(n.Children))
	}
	field, err := fieldPath(n.Children[0])
	if err != nil {
		return nil, err
	}
	arg := n.Children[1]
	if arg.Token == nil || arg.Token.Type != godata.ExpressionTokenString {
		return nil, fmt.Errorf("%w: %s expects a string argument", ErrMalformedFilter, name)
	}
	return &Function{Name: name, Field: field, Arg: unquote(arg.Token.Value)}, nil
}

// fieldPath extracts a (possibly nested) property path from a parse node,
// joining navigation segments with "/". E.g. from/emailAddress/address.
func fieldPath(n *godata.ParseNode) (string, error) {
	parts, err := collectPath(n)
	if err != nil {
		return "", err
	}
	return strings.Join(parts, "/"), nil
}

func collectPath(n *godata.ParseNode) ([]string, error) {
	if n == nil || n.Token == nil {
		return nil, fmt.Errorf("%w: expected a field name", ErrMalformedFilter)
	}
	switch n.Token.Type {
	case godata.ExpressionTokenLiteral:
		return []string{n.Token.Value}, nil
	case godata.ExpressionTokenNav:
		var parts []string
		for _, c := range n.Children {
			p, err := collectPath(c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p...)
		}
		return parts, nil
	default:
		// A non-field operand here is usually an unsupported function or
		// operator nested inside a comparison (e.g. substring(...) eq 'x');
		// surface the precise reason rather than a generic malformed error.
		return nil, classifyUnsupported(n.Token, fmt.Errorf("%w: expected a field name, got %q", ErrMalformedFilter, n.Token.Value))
	}
}

// classifyUnsupported returns ErrUnsupportedFunction or ErrUnsupportedOperator
// when tok denotes a function or operator outside the supported subset; it falls
// back to def for anything else (e.g. a stray literal where a field was wanted).
func classifyUnsupported(tok *godata.Token, def error) error {
	switch tok.Type {
	case godata.ExpressionTokenFunc:
		return fmt.Errorf("%w: %s", ErrUnsupportedFunction, tok.Value)
	case godata.ExpressionTokenLogical, godata.ExpressionTokenOp:
		return fmt.Errorf("%w: %s", ErrUnsupportedOperator, tok.Value)
	default:
		return def
	}
}

// literalFromNode classifies a literal-valued parse node.
func literalFromNode(n *godata.ParseNode) (Literal, error) {
	if n == nil || n.Token == nil {
		return Literal{}, fmt.Errorf("%w: expected a literal value", ErrMalformedFilter)
	}
	switch n.Token.Type {
	case godata.ExpressionTokenString:
		return Literal{Kind: StringLiteral, Value: unquote(n.Token.Value)}, nil
	case godata.ExpressionTokenInteger, godata.ExpressionTokenFloat:
		return Literal{Kind: NumberLiteral, Value: n.Token.Value}, nil
	case godata.ExpressionTokenBoolean:
		return Literal{Kind: BooleanLiteral, Value: n.Token.Value}, nil
	case godata.ExpressionTokenDate, godata.ExpressionTokenTime, godata.ExpressionTokenDateTime:
		return Literal{Kind: DateTimeLiteral, Value: n.Token.Value}, nil
	case godata.ExpressionTokenGuid:
		return Literal{Kind: StringLiteral, Value: n.Token.Value}, nil
	case godata.ExpressionTokenNull:
		return Literal{Kind: NullLiteral, Value: "null"}, nil
	default:
		return Literal{}, fmt.Errorf("%w: expected a literal value, got %q", ErrMalformedFilter, n.Token.Value)
	}
}

// unquote strips the surrounding single quotes from an OData string literal and
// unescapes doubled single quotes (” -> ').
func unquote(s string) string {
	s = strings.TrimPrefix(s, "'")
	s = strings.TrimSuffix(s, "'")
	return strings.ReplaceAll(s, "''", "'")
}

// Fields returns the distinct field paths referenced anywhere in the filter, in
// first-seen order.
func (f *Filter) Fields() []string {
	seen := make(map[string]bool)
	var out []string
	var walk func(Node)
	walk = func(n Node) {
		switch t := n.(type) {
		case *Comparison:
			if !seen[t.Field] {
				seen[t.Field] = true
				out = append(out, t.Field)
			}
		case *Function:
			if !seen[t.Field] {
				seen[t.Field] = true
				out = append(out, t.Field)
			}
		case *Logical:
			for _, o := range t.Operands {
				walk(o)
			}
		}
	}
	if f != nil && f.Root != nil {
		walk(f.Root)
	}
	return out
}

// Validate checks that every field referenced by the filter appears in allowed.
// It returns an error wrapping ErrUnknownField naming the first offending field,
// or nil if every reference is permitted. An empty allow-list rejects any filter
// that references a field.
func (f *Filter) Validate(allowed []string) error {
	permitted := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		permitted[a] = true
	}
	for _, field := range f.Fields() {
		if !permitted[field] {
			return fmt.Errorf("%w: %q", ErrUnknownField, field)
		}
	}
	return nil
}

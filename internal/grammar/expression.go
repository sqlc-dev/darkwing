package grammar

import (
	"fmt"
	"strings"
)

// ExprType classifies a node of a rule's expression tree. Port of duckdb
// PEGExpression::Type (peg_parser.hpp).
type ExprType int

const (
	// ExprLiteral is a quoted keyword or punctuation literal.
	ExprLiteral ExprType = iota
	// ExprReference is a reference to a parameter or another rule.
	ExprReference
	// ExprFunctionCall is a parameterized-rule call; its single child is
	// the argument.
	ExprFunctionCall
	// ExprSequence is an ordered sequence of its children.
	ExprSequence
	// ExprChoice is an ordered choice ('/') between its children.
	ExprChoice
	// ExprOptional is child?.
	ExprOptional
	// ExprRepeat is child+ (one or more).
	ExprRepeat
	// ExprOptionalRepeat is child* (zero or more).
	ExprOptionalRepeat
	// ExprRegex is a character class; the matcher factory rejects it.
	ExprRegex
)

// Expression is one node of a rule's expression tree. Port of duckdb
// PEGExpression.
type Expression struct {
	Type ExprType
	// Text is the literal text, reference name or function name.
	Text string
	// Children holds the operands of sequences, choices, the postfix
	// operators and function calls.
	Children []*Expression
}

// String renders the expression in grammar syntax, parenthesizing every
// nested sequence and choice so the tree structure is explicit.
func (e *Expression) String() string {
	var sb strings.Builder
	e.write(&sb, true)
	return sb.String()
}

func (e *Expression) write(sb *strings.Builder, top bool) {
	join := func(sep string) {
		if !top {
			sb.WriteString("(")
		}
		for i, child := range e.Children {
			if i > 0 {
				sb.WriteString(sep)
			}
			child.write(sb, false)
		}
		if !top {
			sb.WriteString(")")
		}
	}
	switch e.Type {
	case ExprLiteral:
		sb.WriteString("'" + e.Text + "'")
	case ExprReference, ExprRegex:
		sb.WriteString(e.Text)
	case ExprFunctionCall:
		sb.WriteString(e.Text + "(")
		e.Children[0].write(sb, true)
		sb.WriteString(")")
	case ExprSequence:
		join(" ")
	case ExprChoice:
		join(" / ")
	case ExprOptional, ExprRepeat, ExprOptionalRepeat:
		e.Children[0].write(sb, false)
		sb.WriteString(map[ExprType]string{ExprOptional: "?", ExprRepeat: "+", ExprOptionalRepeat: "*"}[e.Type])
	}
}

// buildExpression parses a rule's token stream into its expression tree.
// Port of PEGExpressionParser (peg_parser.cpp), a recursive-descent parser
// with textbook PEG precedence: postfix operators bind tighter than
// sequencing, which binds tighter than '/'. Negative lookahead ('!') is
// parsed but ignored - the operand is matched as if the '!' were absent,
// an acknowledged upstream FIXME replicated here; flip it in lockstep
// with upstream, never independently.
func buildExpression(tokens []Token) (*Expression, error) {
	p := &exprParser{tokens: tokens}
	root, err := p.parseChoice()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, fmt.Errorf("unexpected trailing tokens in PEG rule")
	}
	return root, nil
}

type exprParser struct {
	tokens []Token
	pos    int
}

func (p *exprParser) isOp(op byte) bool {
	return p.pos < len(p.tokens) && p.tokens[p.pos].Type == Operator && p.tokens[p.pos].Text[0] == op
}

// parseChoice: choice := sequence ('/' sequence)*
func (p *exprParser) parseChoice() (*Expression, error) {
	first, err := p.parseSequence()
	if err != nil {
		return nil, err
	}
	if !p.isOp('/') {
		return first, nil
	}
	choice := &Expression{Type: ExprChoice, Children: []*Expression{first}}
	for p.isOp('/') {
		p.pos++
		next, err := p.parseSequence()
		if err != nil {
			return nil, err
		}
		choice.Children = append(choice.Children, next)
	}
	return choice, nil
}

// parseSequence: sequence := postfix*
func (p *exprParser) parseSequence() (*Expression, error) {
	seq := &Expression{Type: ExprSequence}
	for p.pos < len(p.tokens) && !p.isOp('/') && !p.isOp(')') {
		child, err := p.parsePostfix()
		if err != nil {
			return nil, err
		}
		seq.Children = append(seq.Children, child)
	}
	if len(seq.Children) == 1 {
		// collapse trivial sequences
		return seq.Children[0], nil
	}
	return seq, nil
}

// parsePostfix: postfix := primary ('?' | '*' | '+')?
func (p *exprParser) parsePostfix() (*Expression, error) {
	node, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for _, op := range []struct {
		char byte
		typ  ExprType
	}{{'?', ExprOptional}, {'*', ExprOptionalRepeat}, {'+', ExprRepeat}} {
		if p.isOp(op.char) {
			p.pos++
			return &Expression{Type: op.typ, Children: []*Expression{node}}, nil
		}
	}
	return node, nil
}

// parsePrimary: primary := '(' choice ')' | literal | reference | function_call
func (p *exprParser) parsePrimary() (*Expression, error) {
	if p.isOp('(') {
		p.pos++
		inner, err := p.parseChoice()
		if err != nil {
			return nil, err
		}
		if !p.isOp(')') {
			return nil, fmt.Errorf("expected closing ')' in PEG rule")
		}
		p.pos++
		// a parenthesized group is always a sequence, so it compiles to a
		// list matcher of its own
		if inner.Type == ExprSequence {
			return inner, nil
		}
		return &Expression{Type: ExprSequence, Children: []*Expression{inner}}, nil
	}
	if p.isOp('!') {
		// FIXME (upstream): NOT ignored when parsing
		p.pos++
		return p.parsePrimary()
	}
	if p.pos >= len(p.tokens) {
		return nil, fmt.Errorf("unexpected end of PEG rule tokens")
	}
	tok := p.tokens[p.pos]
	p.pos++
	switch tok.Type {
	case Literal:
		return &Expression{Type: ExprLiteral, Text: tok.Text}, nil
	case Reference:
		return &Expression{Type: ExprReference, Text: tok.Text}, nil
	case FunctionCall:
		// same "consume until close" as '(' does
		body, err := p.parseChoice()
		if err != nil {
			return nil, err
		}
		if !p.isOp(')') {
			return nil, fmt.Errorf("expected closing ')' after function call '%s'", tok.Text)
		}
		p.pos++
		return &Expression{Type: ExprFunctionCall, Text: tok.Text, Children: []*Expression{body}}, nil
	case Regex:
		return &Expression{Type: ExprRegex, Text: tok.Text}, nil
	default:
		return nil, fmt.Errorf("unrecognized peg token type")
	}
}

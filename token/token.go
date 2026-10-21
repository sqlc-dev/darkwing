// Package token defines the lexical tokens of the DuckDB SQL dialect as
// produced by darkwing's tokenizer, plus DuckDB's five-category keyword
// classification. The kinds mirror the parser-relevant subset of DuckDB's
// TokenType (src/include/duckdb/parser/peg/token_type.hpp).
package token

import "fmt"

// Kind classifies a token.
type Kind uint8

const (
	// Invalid is the zero Kind; the tokenizer never emits it.
	Invalid Kind = iota
	// Keyword is a bare word that appears in one of the keyword lists.
	Keyword
	// String is a string literal, including prefixed (E'...', X'...',
	// N'...', B'...') and dollar-quoted forms (kept raw; the string
	// literal matcher strips the delimiters).
	String
	// Number is a numeric literal.
	Number
	// Operator is an operator token (including '(' ')' ',' '$' '?' etc.).
	Operator
	// Identifier is a bare word that is not a keyword, or a quoted
	// identifier (quotes retained in Text; the matcher unwraps them).
	Identifier
	// Comment is a comment token. The parsing tokenizer filters comments
	// out; the Kind exists to mirror upstream's classification.
	Comment
	// Terminator is a statement-terminating ';'. The PEG grammar consumes
	// it (TopLevelStatement); statement boundaries are decided by the
	// grammar, not the tokenizer.
	Terminator
	// EndOfInput is the sentinel appended after the last real token.
	EndOfInput
)

func (k Kind) String() string {
	switch k {
	case Invalid:
		return "INVALID"
	case Keyword:
		return "KEYWORD"
	case String:
		return "STRING_LITERAL"
	case Number:
		return "NUMBER_LITERAL"
	case Operator:
		return "OPERATOR"
	case Identifier:
		return "IDENTIFIER"
	case Comment:
		return "COMMENT"
	case Terminator:
		return "TERMINATOR"
	case EndOfInput:
		return "END_OF_INPUT"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// Span is a half-open byte range [Start, End) into the original SQL text.
type Span struct {
	Start int
	End   int
}

// Token is a single lexical token: Text is the raw source slice. Port of
// upstream's MatcherToken.
type Token struct {
	Kind Kind
	Text string
	Span Span
	// Unterminated marks a token that ran into end of input (an unclosed
	// dollar-quoted string).
	Unterminated bool
	// PrecededByNewline reports a newline between the previous token and
	// this one; PrecededByBlockComment reports a /* */ comment there. They
	// decide string literal continuation ('a'<newline>'b' is 'ab').
	// Upstream sets both only for tokens that go through
	// TokenizerBehavior::PushToken, so single-byte operators, terminators
	// and the final token of the input never carry them.
	PrecededByNewline      bool
	PrecededByBlockComment bool
}

func (t Token) String() string {
	return fmt.Sprintf("%s(%q)@%d", t.Kind, t.Text, t.Span.Start)
}

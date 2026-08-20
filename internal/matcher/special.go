package matcher

import (
	"strings"

	"github.com/sqlc-dev/darkwing/token"
)

// identifierClass selects which keyword category an identifier position
// may consume, mirroring the relevant part of upstream's SuggestionState
// (IdentifierMatcher::GetAllowedCategory / SupportsStringLiteral).
type identifierClass uint8

const (
	// classColumnName allows unreserved + column-name keywords (the
	// default for ColId-style positions).
	classColumnName identifierClass = iota
	// classTypeFunc allows unreserved + func-name/type-name keywords
	// (function name positions).
	classTypeFunc
	// classTypeName allows unreserved + type-name keywords (type name
	// positions).
	classTypeName
)

// identifierMatcher matches identifier tokens with category-aware keyword
// acceptance. Port of IdentifierMatcher and ReservedIdentifierMatcher
// (matcher/identifier_matcher.hpp); reserved selects the latter, which
// accepts even reserved keywords (qualified positions such as
// catalog.schema."select").
type identifierMatcher struct {
	matcherMeta
	class identifierClass
	// reserved: ReservedIdentifierMatcher behavior (no keyword filtering).
	reserved bool
	// supportsStringLiteral: single-quoted tokens are acceptable (table
	// and file name positions, where '...' is a path).
	supportsStringLiteral bool
	// display is the ToString label (upstream prints the suggestion type).
	display string
}

func isQuoted(text string) bool {
	return text[0] == '"' && text[len(text)-1] == '"'
}

func isSingleQuoted(text string) bool {
	return text[0] == '\'' && text[len(text)-1] == '\''
}

// charIsKeywordStart mirrors Tokenizer::CharacterIsKeyword for the first
// identifier character.
func charIsKeywordStart(c byte) bool {
	switch c {
	case '(', ')', '{', '}', '[', ']', ',', '?', '$', '-', '#':
		// single-byte operators
		return false
	case '\'', ';', '"', '.':
		// control flow ('-' already covered above)
		return false
	}
	if c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r' {
		return false
	}
	// StringUtil::CharacterIsOperator: ASCII punctuation except '_'
	if c != '_' && ((c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')) {
		return false
	}
	return true
}

func (m *identifierMatcher) isIdentifier(text string) bool {
	if text == "" {
		return false
	}
	if isSingleQuoted(text) && m.supportsStringLiteral {
		return true
	}
	if isQuoted(text) {
		return true
	}
	if text[0] == '.' || (text[0] >= '0' && text[0] <= '9') {
		// CharacterIsInitialNumber
		return false
	}
	return charIsKeywordStart(text[0])
}

func (m *identifierMatcher) isAllowedKeyword(text string) bool {
	categories := token.Categories(text)
	if categories == token.KeywordNone {
		return true
	}
	if categories.Has(token.KeywordUnreserved) {
		return true
	}
	switch m.class {
	case classTypeName:
		return categories.Has(token.KeywordTypeName)
	case classTypeFunc:
		// upstream's typefunc map is the union of the func-name and
		// type-name lists
		return categories.Has(token.KeywordFuncName) || categories.Has(token.KeywordTypeName)
	default:
		return categories.Has(token.KeywordColumnName)
	}
}

func (m *identifierMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil {
		return nil
	}
	text := tok.Text
	if !m.reserved && !m.isAllowedKeyword(text) {
		return nil
	}
	if !m.isIdentifier(text) {
		return nil
	}
	offset := tok.Span.Start
	length := len(text)
	s.advance()

	result := text
	if m.reserved {
		// ReservedIdentifierMatcher only unwraps double quotes; a
		// single-quoted path literal is kept verbatim
		if isQuoted(result) {
			result = strings.ReplaceAll(result[1:len(result)-1], `""`, `"`)
		}
	} else {
		switch {
		case isQuoted(result):
			result = strings.ReplaceAll(result[1:len(result)-1], `""`, `"`)
		case isSingleQuoted(result) && m.supportsStringLiteral:
			// a single-quoted token in a table or file-name position is a
			// path: unwrapped but never folded
			result = strings.ReplaceAll(result[1:len(result)-1], "''", "'")
		}
	}
	// identifier case folding (preserve_identifier_case) is not ported:
	// darkwing always preserves case, DuckDB's default

	ir := &IdentifierResult{Identifier: result}
	ir.offset, ir.hasOffset = offset, true
	ir.length, ir.hasLength = length, true
	return ir
}

func (m *identifierMatcher) String() string {
	return m.display
}

// numberLiteralMatcher matches number literal tokens. Port of
// NumberLiteralMatcher.
type numberLiteralMatcher struct {
	matcherMeta
}

func charIsInitialNumber(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.'
}

func charIsNumberCont(c byte) bool {
	return charIsInitialNumber(c) || c == 'e' || c == 'E' || c == '_'
}

func (m *numberLiteralMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil {
		return nil
	}
	text := tok.Text
	if text == "" || !charIsInitialNumber(text[0]) {
		return nil
	}
	// a lone '.' is a dot operator, not a number literal
	if text == "." {
		return nil
	}
	scientific := false
	for i := 1; i < len(text); i++ {
		c := text[i]
		if c == 'e' || c == 'E' {
			if scientific {
				// upstream throws a ParserException here, aborting the
				// entire parse rather than failing this alternative
				panic(&MatchError{Msg: "Already found scientific notation"})
			}
			scientific = true
		}
		if scientific && (c == '+' || c == '-') {
			continue
		}
		if !charIsNumberCont(c) {
			return nil
		}
	}
	offset := tok.Span.Start
	length := len(text)
	s.advance()
	nr := &NumberResult{Number: text}
	nr.name = m.name
	nr.offset, nr.hasOffset = offset, true
	nr.length, nr.hasLength = length, true
	return nr
}

func (m *numberLiteralMatcher) String() string {
	return "NUMBER_LITERAL"
}

// stringLiteralMatcher matches string literal tokens, including prefixed
// forms (E”, N”, X”, B”). Port of StringLiteralMatcher +
// special_string_utils.hpp.
type stringLiteralMatcher struct {
	matcherMeta
}

func specialStringInfo(text string) (StringKind, int) {
	if text == "" {
		return StringStandard, 0
	}
	c := text[0]
	if c == '\'' {
		return StringStandard, 1
	}
	// the second char must be a quote to confirm a special string
	if len(text) > 1 && text[1] == '\'' {
		switch c {
		case 'N', 'n':
			return StringNational, 2
		case 'X', 'x':
			return StringHexadecimal, 2
		case 'B', 'b':
			return StringBit, 2
		case 'E', 'e':
			return StringEscape, 2
		}
	}
	return StringStandard, 1
}

func (m *stringLiteralMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil {
		return nil
	}
	text := tok.Text
	kind, prefixLen := specialStringInfo(text)
	if prefixLen < 1 || len(text) < prefixLen+1 {
		return nil
	}
	if text[prefixLen-1] != '\'' || text[len(text)-1] != '\'' {
		return nil
	}
	offset := tok.Span.Start
	length := len(text)
	s.advance()
	value := strings.ReplaceAll(text[prefixLen:len(text)-1], "''", "'")
	sr := &StringResult{Value: value, StringKind: kind}
	sr.name = m.name
	sr.offset, sr.hasOffset = offset, true
	sr.length, sr.hasLength = length, true
	return sr
}

func (m *stringLiteralMatcher) String() string {
	return "STRING_LITERAL"
}

// operatorMatcher matches generic multi-character operator tokens that are
// not claimed by dedicated grammar rules. Port of OperatorMatcher.
type operatorMatcher struct {
	matcherMeta
}

func isOperatorChar(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '%', '^', '<', '>', '=', '~', '!', '@', '&', '|':
		return true
	default:
		return false
	}
}

func (m *operatorMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil {
		return nil
	}
	text := tok.Text
	// exclude the lambda arrow and JSON arrow - these have dedicated
	// grammar roles
	if text == "->" || text == "->>" {
		return nil
	}
	// single-character operators are handled at specific precedence levels
	if len(text) == 1 {
		return nil
	}
	// exclude known comparison operators - handled by ComparisonExpression
	switch text {
	case "<=", ">=", "!=", "==", "<>":
		return nil
	}
	// exclude LIKE/SIMILAR operators - handled by LikeVariations
	switch text {
	case "~~", "~~*", "~~~", "~*", "!~~", "!~~*", "!~", "!~*":
		return nil
	}
	for i := 0; i < len(text); i++ {
		if !isOperatorChar(text[i]) {
			return nil
		}
	}
	offset := tok.Span.Start
	length := len(text)
	s.advance()
	or := &OperatorResult{Operator: text}
	or.offset, or.hasOffset = offset, true
	or.length, or.hasLength = length, true
	return or
}

func (m *operatorMatcher) String() string {
	return "OPERATOR"
}

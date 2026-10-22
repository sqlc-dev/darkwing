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
	// classBareLabel allows only bare-label keywords (column aliases
	// without AS); unlike the other classes, unreserved keywords outside
	// the bare-label list are rejected.
	classBareLabel
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

// isQuoteDelimiter is IdentifierMatcher::IsQuoteDelimiter.
func isQuoteDelimiter(c byte) bool {
	return c == '"' || c == '`'
}

// isQuoted reports a delimited identifier: "..." or `...`, closed by the
// delimiter it opened with.
func isQuoted(text string) bool {
	if len(text) < 2 {
		return false
	}
	return isQuoteDelimiter(text[0]) && text[0] == text[len(text)-1]
}

// unquoteIdentifier strips the delimiters of a quoted identifier and
// un-doubles escaped delimiters (IdentifierMatcher::UnquoteIdentifier).
func unquoteIdentifier(text string) string {
	delimiter := text[:1]
	return strings.ReplaceAll(text[1:len(text)-1], delimiter+delimiter, delimiter)
}

func isSingleQuoted(text string) bool {
	return text[0] == '\'' && text[len(text)-1] == '\''
}

// charIsKeywordStart mirrors Tokenizer::CharacterIsKeyword for the first
// identifier character.
func charIsKeywordStart(c byte) bool {
	switch c {
	case '(', ')', '{', '}', '[', ']', ',', ':', '?', '$', '#':
		// single-byte operators
		return false
	case '\'', ';', '"', '.':
		// control flow
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
	if text[len(text)-1] == '\'' {
		// a prefixed string literal such as x'41' or e'ab'
		return false
	}
	return charIsKeywordStart(text[0])
}

func (m *identifierMatcher) isAllowedKeyword(text string) bool {
	categories := token.Categories(text)
	if categories == token.KeywordNone {
		return true
	}
	// DefaultKeywordMaps::GetIdentifierMask
	if m.class == classBareLabel {
		return categories.Has(token.KeywordBareLabel)
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
		// ReservedIdentifierMatcher only unwraps quoted identifiers; a
		// single-quoted path literal is kept verbatim
		if isQuoted(result) {
			result = unquoteIdentifier(result)
		}
	} else {
		switch {
		case isQuoted(result):
			result = unquoteIdentifier(result)
		case isSingleQuoted(result) && m.supportsStringLiteral:
			// a single-quoted token in a table or file-name position is a
			// path: unwrapped but never folded
			result = strings.ReplaceAll(result[1:len(result)-1], "''", "'")
		}
	}
	// identifier case folding (MatchState::FoldIdentifier) is not ported:
	// darkwing always preserves case, DuckDB's default
	// (IdentifierCaseMode::PRESERVE_CASE)

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
	hasDot := text[0] == '.'
	for i := 1; i < len(text); i++ {
		c := text[i]
		if c == '.' {
			// a number has at most one dot, and none in its exponent
			if hasDot || scientific {
				return nil
			}
			hasDot = true
		}
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

// stringLiteralMatcher matches string literal tokens: single-quoted,
// including the prefixed forms (E'...', N'...', X'...', B'...'), and
// dollar-quoted.
// A single-quoted literal absorbs the plain '...' literals that follow it
// on later lines ('a'<newline>'b' is 'ab'). Port of StringLiteralMatcher
// (matcher/string_literal_matcher.hpp) + special_string_utils.hpp.
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

// isSingleQuotedLiteral is StringLiteralMatcher::IsSingleQuotedStringLiteral.
func isSingleQuotedLiteral(text string, prefixLen int) bool {
	if prefixLen == 0 || len(text) < prefixLen+1 {
		return false
	}
	return text[prefixLen-1] == '\'' && text[len(text)-1] == '\''
}

// dollarQuoteDelimiterLength reports the length of the $tag$ delimiter of
// a terminated dollar-quoted literal (TryGetDollarQuoteDelimiterLength).
func dollarQuoteDelimiterLength(tok *token.Token) (int, bool) {
	text := tok.Text
	if tok.Unterminated || text == "" || text[0] != '$' {
		return 0, false
	}
	end := strings.IndexByte(text[1:], '$')
	if end < 0 {
		return 0, false
	}
	length := end + 2
	if len(text) < length*2 {
		return 0, false
	}
	return length, strings.HasSuffix(text, text[:length])
}

// stripStringLiteral is StringLiteralMatcher::StripStringLiteral: the
// contents between the delimiters, with doubled quotes un-doubled in
// single-quoted literals.
func stripStringLiteral(tok *token.Token, prefixLen int) string {
	if length, ok := dollarQuoteDelimiterLength(tok); ok {
		return tok.Text[length : len(tok.Text)-length]
	}
	return strings.ReplaceAll(tok.Text[prefixLen:len(tok.Text)-1], "''", "'")
}

// isStringContinuation is StringLiteralMatcher::IsStringLiteralContinuation:
// a plain single-quoted literal on a later line, with no block comment in
// between.
func isStringContinuation(tok *token.Token) bool {
	if tok == nil || !tok.PrecededByNewline || tok.PrecededByBlockComment || tok.Kind != token.String {
		return false
	}
	kind, prefixLen := specialStringInfo(tok.Text)
	return kind == StringStandard && isSingleQuotedLiteral(tok.Text, prefixLen)
}

func (m *stringLiteralMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil {
		return nil
	}
	kind, prefixLen := specialStringInfo(tok.Text)
	singleQuoted := isSingleQuotedLiteral(tok.Text, prefixLen)
	if _, dollar := dollarQuoteDelimiterLength(tok); !singleQuoted && !dollar {
		return nil
	}
	offset := tok.Span.Start
	end := tok.Span.Start + len(tok.Text)
	value := stripStringLiteral(tok, prefixLen)
	s.advance()
	if singleQuoted {
		for next := s.current(); isStringContinuation(next); next = s.current() {
			_, nextPrefix := specialStringInfo(next.Text)
			value += stripStringLiteral(next, nextPrefix)
			end = next.Span.Start + len(next.Text)
			s.advance()
		}
	}
	sr := &StringResult{Value: value, StringKind: kind}
	sr.name = m.name
	sr.offset, sr.hasOffset = offset, true
	sr.length, sr.hasLength = end-offset, true
	return sr
}

func (m *stringLiteralMatcher) String() string {
	return "STRING_LITERAL"
}

// operatorMatcher matches operator tokens. Port of OperatorMatcher
// (matcher/operator_matcher.hpp): in the default GENERIC_PRECEDENCE mode
// (OperatorLiteral) it skips the operators the grammar gives a precedence
// level of their own; in ALL_OPERATORS mode (AnyOperatorLiteral, the
// operator of ANY/ALL comparisons) it accepts every operator.
type operatorMatcher struct {
	matcherMeta
	allOperators bool
}

// hasSpecialPrecedence is OperatorMatcher::HasSpecialPrecedence
// (operator_matcher.cpp): the comparison, LIKE-family and lambda-arrow
// operators.
func hasSpecialPrecedence(op string) bool {
	switch op {
	case "=", "<", ">", "~",
		"->", "<=", ">=", "!=", "==", "<>", "~~", "~*", "!~",
		"~~*", "~~~", "!~~", "!~*",
		"!~~*":
		return true
	default:
		return false
	}
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
	if !m.allOperators && hasSpecialPrecedence(text) {
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

// Package lexer tokenizes DuckDB SQL. It is a port of DuckDB's parsing
// tokenizer: src/parser/peg/tokenizer/base_tokenizer.cpp with the parser
// behavior overrides from parser_tokenizer.cpp. The PEG matcher is
// token-level, not character-level: this tokenizer runs first, strips
// comments, and emits tokens with byte offsets; the matcher never sees raw
// text.
package lexer

import (
	"fmt"
	"strings"

	"github.com/sqlc-dev/darkwing/token"
)

// Error is a tokenization error with a byte offset into the input.
type Error struct {
	Msg    string
	Offset int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (offset %d)", e.Msg, e.Offset)
}

type tokenizeState int

const (
	stateStandard tokenizeState = iota
	stateSingleLineComment
	stateMultiLineComment
	stateQuotedIdentifier
	stateStringLiteral
	stateKeyword
	stateNumeric
	stateOperator
	stateDollarQuotedString
)

func stateToKind(state tokenizeState) token.Kind {
	switch state {
	case stateStandard:
		return token.Identifier
	case stateSingleLineComment, stateMultiLineComment:
		return token.Comment
	case stateQuotedIdentifier:
		return token.Identifier
	case stateStringLiteral:
		return token.String
	case stateKeyword:
		return token.Keyword
	case stateNumeric:
		return token.Number
	case stateOperator:
		return token.Operator
	case stateDollarQuotedString:
		return token.String
	default:
		panic("unknown tokenize state")
	}
}

func isUnterminatedState(state tokenizeState) bool {
	switch state {
	case stateStringLiteral, stateQuotedIdentifier, stateDollarQuotedString:
		return true
	default:
		return false
	}
}

func charIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func charIsDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// charIsOperatorAny is StringUtil::CharacterIsOperator: any ASCII
// punctuation except underscore.
func charIsOperatorAny(c byte) bool {
	if c == '_' {
		return false
	}
	if c >= '!' && c <= '/' {
		return true
	}
	if c >= ':' && c <= '@' {
		return true
	}
	if c >= '[' && c <= '`' {
		return true
	}
	if c >= '{' && c <= '~' {
		return true
	}
	return false
}

func isSingleByteOperator(c byte) bool {
	switch c {
	case '(', ')', '{', '}', '[', ']', ',', '?', '$', '-', '#':
		return true
	default:
		return false
	}
}

func charIsInitialNumber(c byte) bool {
	return charIsDigit(c) || c == '.'
}

func charIsNumber(c byte) bool {
	if charIsInitialNumber(c) {
		return true
	}
	switch c {
	case 'e', 'E', '_': // exponents and separators
		return true
	default:
		return false
	}
}

func charIsScientific(c byte) bool {
	return c == 'e' || c == 'E'
}

func charIsControlFlow(c byte) bool {
	switch c {
	case '\'', '-', ';', '"', '.':
		return true
	default:
		return false
	}
}

func charIsKeyword(c byte) bool {
	if isSingleByteOperator(c) {
		return false
	}
	if charIsOperatorAny(c) {
		return false
	}
	if charIsSpace(c) {
		return false
	}
	if charIsControlFlow(c) {
		return false
	}
	return true
}

// charIsOperatorCont reports whether c may continue a multi-character
// operator token (Tokenizer::CharacterIsOperator).
func charIsOperatorCont(c byte) bool {
	if isSingleByteOperator(c) {
		return false
	}
	if charIsControlFlow(c) {
		return false
	}
	return charIsOperatorAny(c)
}

func charIsSpecialStringPrefix(c byte) bool {
	switch c {
	case 'N', 'n', 'X', 'x', 'E', 'e', 'B', 'b':
		return true
	default:
		return false
	}
}

// isValidDollarTagChar reports whether c may appear in a dollar-quote tag:
// A-Z, a-z, 0-9, underscore, or \200-\377. Digits are only valid after the
// first character; callers validate that before scanning.
func isValidDollarTagChar(c byte) bool {
	if c >= 'A' && c <= 'Z' {
		return true
	}
	if c >= 'a' && c <= 'z' {
		return true
	}
	if c >= '0' && c <= '9' {
		return true
	}
	if c == '_' {
		return true
	}
	return c >= 0x80
}

// specialOperator reports whether a multi-character special operator starts
// at sql[pos] and returns its length (Tokenizer::IsSpecialOperator).
func specialOperator(sql string, pos int) (int, bool) {
	if pos+2 < len(sql) && sql[pos:pos+3] == "->>" {
		return 3, true
	}
	if pos+1 >= len(sql) {
		// 2-byte operators are out-of-bounds
		return 0, false
	}
	switch sql[pos : pos+2] {
	case "::", ":=", "->", "**", "//":
		return 2, true
	}
	return 0, false
}

type lexer struct {
	sql    string
	tokens []token.Token
}

func (l *lexer) push(kind token.Kind, text string, offset int) {
	l.tokens = append(l.tokens, token.Token{
		Kind: kind,
		Text: text,
		Span: token.Span{Start: offset, End: offset + len(text)},
	})
}

// pushRange is TokenizerBehavior::PushToken with the parser behavior's
// zero-length delimited identifier check: comments and empty ranges are
// dropped.
func (l *lexer) pushRange(start, end int, kind token.Kind, unterminated bool) error {
	if kind == token.Identifier && end == start+2 && l.sql[start:end] == `""` {
		return &Error{Msg: "zero-length delimited identifier", Offset: start}
	}
	if kind == token.Comment {
		return nil
	}
	if start >= end {
		return nil
	}
	l.tokens = append(l.tokens, token.Token{
		Kind:         kind,
		Text:         l.sql[start:end],
		Span:         token.Span{Start: start, End: end},
		Unterminated: unterminated,
	})
	return nil
}

// Tokenize splits sql into tokens for the PEG matcher. Comments are
// filtered out; every ';' becomes a Terminator token (statement boundaries
// are determined by the grammar, not the tokenizer); an EndOfInput sentinel
// is always appended. Errors carry byte offsets and match the conditions
// upstream's parser tokenizer raises: unterminated string literals,
// unterminated quoted identifiers, and zero-length delimited identifiers.
func Tokenize(sql string) ([]token.Token, error) {
	l := &lexer{sql: sql}
	if err := l.run(); err != nil {
		return nil, err
	}
	l.push(token.EndOfInput, "", len(sql))
	return l.tokens, nil
}

func (l *lexer) run() error {
	sql := l.sql
	n := len(sql)
	state := stateStandard
	lastPos := 0
	escapeString := false
	dollarQuoteMarker := ""
	dollarMarkerStart := 0
	multiLineCommentDepth := 0

	for i := 0; i < n; i++ {
		c := sql[i]
		switch state {
		case stateStandard:
			if c == '\'' {
				state = stateStringLiteral
				lastPos = i
				escapeString = false
				break
			}
			if c == '"' {
				state = stateQuotedIdentifier
				lastPos = i
				break
			}
			if c == ';' {
				// end of statement - always emitted as a Terminator token
				// so the grammar can consume it
				l.push(token.Terminator, ";", i)
				lastPos = i + 1
				break
			}
			if c == '$' {
				// dollar-quoted string, parameter, or collabel parameter
				if i+1 >= n {
					// we need more than a single dollar
					break
				}
				if sql[i+1] >= '0' && sql[i+1] <= '9' {
					// $[numeric] is a parameter, not a dollar-quoted string
					l.push(token.Operator, "$", i)
					break
				}
				// scan until next $
				nextDollar := 0
				for idx := i + 1; idx < n; idx++ {
					if sql[idx] == '$' {
						nextDollar = idx
						break
					}
					if !isValidDollarTagChar(sql[idx]) {
						break
					}
				}
				if nextDollar == 0 {
					// collabel parameter ($collabel)
					l.push(token.Operator, "$", i)
					break
				}
				state = stateDollarQuotedString
				lastPos = i
				i = nextDollar
				// found a complete marker, store it
				dollarMarkerStart = lastPos + 1
				dollarQuoteMarker = sql[dollarMarkerStart:i]
				break
			}
			if c == '-' && i+1 < n && sql[i+1] == '-' {
				i++
				state = stateSingleLineComment
				break
			}
			if c == '/' && i+1 < n && sql[i+1] == '*' {
				i++
				state = stateMultiLineComment
				multiLineCommentDepth = 1
				break
			}
			if charIsSpace(c) {
				lastPos = i + 1
				break
			}
			if opLen, ok := specialOperator(sql, i); ok {
				// special operator - pushed with lastPos as its offset,
				// exactly as upstream does
				l.push(token.Operator, sql[i:i+opLen], lastPos)
				i += opLen - 1
				lastPos = i + 1
				break
			}
			if isSingleByteOperator(c) {
				l.push(token.Operator, string(c), lastPos)
				lastPos = i + 1
				break
			}
			if charIsInitialNumber(c) {
				state = stateNumeric
				lastPos = i
				break
			}
			if charIsSpecialStringPrefix(c) {
				// look ahead to see if a quote follows
				if i+1 < n && sql[i+1] == '\'' {
					state = stateStringLiteral
					lastPos = i
					if c == 'E' || c == 'e' {
						escapeString = true
					}
					i++
					break
				}
			}
			if charIsOperatorAny(c) {
				state = stateOperator
				lastPos = i
				break
			}
			state = stateKeyword
			lastPos = i

		case stateNumeric:
			// "always allowed" numeric characters
			if charIsInitialNumber(c) {
				break
			}
			// underscore only when immediately followed by a digit
			if c == '_' && i+1 < n && charIsInitialNumber(sql[i+1]) {
				break
			}
			// scientific notation marker
			if charIsScientific(c) && !charIsScientific(sql[i-1]) {
				// require at least one digit before 'e'/'E'
				if charIsDigit(sql[lastPos]) || charIsDigit(sql[i-1]) {
					break
				}
			}
			// '+' or '-' is only valid directly after the exponent marker
			if (c == '+' || c == '-') && charIsScientific(sql[i-1]) {
				break
			}
			// end of number: backtrack to the last plain numeric character
			for !charIsInitialNumber(sql[i-1]) {
				i--
			}
			if err := l.pushRange(lastPos, i, token.Number, false); err != nil {
				return err
			}
			state = stateStandard
			lastPos = i
			i--

		case stateOperator:
			if !charIsOperatorCont(c) {
				// PostgreSQL trimming rule: an operator cannot end in '+'
				// unless it contains at least one of: ~ ! @ # % ^ & | ` ?
				endPos := i
				hasSpecial := false
				for j := lastPos; j < endPos; j++ {
					switch sql[j] {
					case '~', '!', '@', '#', '%', '^', '&', '|', '`', '?':
						hasSpecial = true
					}
					if hasSpecial {
						break
					}
				}
				if !hasSpecial {
					for endPos > lastPos && sql[endPos-1] == '+' {
						endPos--
					}
				}
				if err := l.pushRange(lastPos, endPos, token.Operator, false); err != nil {
					return err
				}
				// push any trimmed '+' characters as individual tokens
				for j := endPos; j < i; j++ {
					l.push(token.Operator, string(sql[j]), j)
				}
				state = stateStandard
				lastPos = i
				i--
			}

		case stateKeyword:
			// '$' is valid as a non-initial identifier character
			if c != '$' && !charIsKeyword(c) {
				word := sql[lastPos:i]
				kind := token.Identifier
				if token.IsKeyword(word) {
					kind = token.Keyword
				}
				if err := l.pushRange(lastPos, i, kind, false); err != nil {
					return err
				}
				state = stateStandard
				lastPos = i
				i--
			}

		case stateStringLiteral:
			if escapeString && c == '\\' && i+1 < n {
				i++
				break
			}
			if c == '\'' {
				if i+1 < n && sql[i+1] == '\'' {
					// escaped - skip escape
					i++
				} else {
					if err := l.pushRange(lastPos, i+1, token.String, false); err != nil {
						return err
					}
					lastPos = i + 1
					escapeString = false
					state = stateStandard
				}
			}

		case stateQuotedIdentifier:
			if c == '"' {
				if i+1 < n && sql[i+1] == '"' {
					// escaped - skip escape
					i++
				} else {
					if err := l.pushRange(lastPos, i+1, token.Identifier, false); err != nil {
						return err
					}
					lastPos = i + 1
					state = stateStandard
				}
			}

		case stateSingleLineComment:
			if c == '\n' || c == '\r' {
				if err := l.pushRange(lastPos, i+1, token.Comment, false); err != nil {
					return err
				}
				lastPos = i + 1
				state = stateStandard
			}

		case stateMultiLineComment:
			if c == '/' && i+1 < n && sql[i+1] == '*' {
				i++
				multiLineCommentDepth++
			} else if c == '*' && i+1 < n && sql[i+1] == '/' {
				i++
				multiLineCommentDepth--
				if multiLineCommentDepth == 0 {
					if err := l.pushRange(lastPos, i+1, token.Comment, false); err != nil {
						return err
					}
					lastPos = i + 1
					state = stateStandard
				}
			}

		case stateDollarQuotedString:
			// all that will get us out is a $[marker]$
			if c != '$' {
				break
			}
			if i+1 >= n {
				// no room for the final dollar
				break
			}
			// skip to the next dollar symbol
			start := i + 1
			end := start
			for end < n && sql[end] != '$' {
				end++
			}
			if end >= n {
				// no final dollar, continue as normal
				break
			}
			if end-start != len(dollarQuoteMarker) {
				// length mismatch, cannot match
				break
			}
			if sql[start:start+len(dollarQuoteMarker)] != dollarQuoteMarker {
				// marker mismatch
				break
			}
			// marker found - rewrite to a plain quoted literal, exactly as
			// upstream does
			fullMarkerLen := len(dollarQuoteMarker) + 2
			quoted := sql[lastPos : start+len(dollarQuoteMarker)+1]
			content := quoted[fullMarkerLen : len(quoted)-fullMarkerLen]
			content = strings.ReplaceAll(content, "'", "''")
			quoted = "'" + content + "'"
			// upstream records the rewritten text's length, not the source
			// extent, so the span is Start + len(Text)
			l.push(token.String, quoted, dollarMarkerStart-1)
			dollarQuoteMarker = ""
			state = stateStandard
			i = end
			lastPos = i + 1
		}
	}

	switch state {
	case stateSingleLineComment, stateMultiLineComment:
		return l.pushRange(lastPos, n, token.Comment, false)
	case stateDollarQuotedString:
		return l.pushRange(lastPos, n, token.String, true)
	}

	// last token handling (parser behavior: unterminated strings and quoted
	// identifiers are errors)
	switch state {
	case stateStringLiteral:
		return &Error{Msg: "unterminated string literal", Offset: lastPos}
	case stateQuotedIdentifier:
		return &Error{Msg: "unterminated quoted identifier", Offset: lastPos}
	}
	lastWord := sql[lastPos:]
	if len(lastWord) == 0 {
		return nil
	}
	if state == stateKeyword && !token.IsKeyword(lastWord) {
		state = stateStandard
	}
	return l.pushRange(lastPos, n, stateToKind(state), isUnterminatedState(state))
}

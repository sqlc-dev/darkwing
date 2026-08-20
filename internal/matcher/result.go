// Package matcher compiles the loaded PEG grammar into a matcher tree and
// matches token streams against it, producing a generic parse tree
// (ParseResult). It is a port of DuckDB's matcher_factory.cpp, matcher.cpp,
// parser_packrat.cpp and the matcher/*.hpp headers, minus the autocomplete
// suggestion machinery (out of scope for darkwing).
package matcher

import (
	"fmt"
	"strings"
)

// ResultKind classifies a ParseResult node. Port of duckdb ParseResultType
// (transformer/parse_result.hpp), restricted to the kinds the matcher
// produces.
type ResultKind uint8

const (
	// KindList is a matched sequence.
	KindList ResultKind = iota
	// KindOptional is a matched `?` (possibly empty).
	KindOptional
	// KindRepeat is a matched `+` body (also the body of `*`, which
	// compiles to Optional(Repeat(...))).
	KindRepeat
	// KindChoice is a matched ordered-choice alternative.
	KindChoice
	// KindIdentifier is a matched identifier special token.
	KindIdentifier
	// KindKeyword is a matched keyword or punctuation literal.
	KindKeyword
	// KindNumber is a matched number literal special token.
	KindNumber
	// KindString is a matched string literal special token.
	KindString
	// KindOperator is a matched operator special token.
	KindOperator
	// KindEndOfInput is the consumed end-of-input sentinel.
	KindEndOfInput
)

func (k ResultKind) String() string {
	switch k {
	case KindList:
		return "LIST"
	case KindOptional:
		return "OPTIONAL"
	case KindRepeat:
		return "REPEAT"
	case KindChoice:
		return "CHOICE"
	case KindIdentifier:
		return "IDENTIFIER"
	case KindKeyword:
		return "KEYWORD"
	case KindNumber:
		return "NUMBER"
	case KindString:
		return "STRING"
	case KindOperator:
		return "OPERATOR"
	case KindEndOfInput:
		return "END_OF_INPUT"
	default:
		return fmt.Sprintf("ResultKind(%d)", uint8(k))
	}
}

// StringKind classifies a string literal by its prefix. Port of duckdb
// SpecialStringCharacter (special_string_utils.hpp).
type StringKind uint8

const (
	// StringStandard is a plain '...' literal.
	StringStandard StringKind = iota
	// StringNational is an N'...' literal.
	StringNational
	// StringHexadecimal is an X'...' literal.
	StringHexadecimal
	// StringEscape is an E'...' literal.
	StringEscape
	// StringBit is a B'...' literal.
	StringBit
)

// ParseResult is one node of the generic parse tree. Nodes are immutable
// once matching completes; packrat memoization may share a node between
// multiple parents.
type ParseResult interface {
	Kind() ResultKind
	// Name is the grammar rule name that produced this node, or "" for
	// anonymous sub-rules (e.g. a parenthesized group).
	Name() string
	// Loc returns the source byte range [offset, offset+length) this node
	// covers. ok is false when the node has no location (an empty optional,
	// or end of input).
	Loc() (offset, length int, ok bool)

	base() *resultBase
	dump(sb *strings.Builder, visited map[*resultBase]bool, indent string, isLast bool)
}

type resultBase struct {
	name      string
	offset    int
	length    int
	hasOffset bool
	hasLength bool
}

func (b *resultBase) Name() string { return b.name }

func (b *resultBase) Loc() (int, int, bool) {
	if !b.hasOffset {
		return 0, 0, false
	}
	length := 0
	if b.hasLength {
		length = b.length
	}
	return b.offset, length, true
}

func (b *resultBase) base() *resultBase { return b }

// encloseChild grows this node's location so it encloses the child's
// location. Port of ParseResult::EncloseChild + QueryLocation::Merge.
func (b *resultBase) encloseChild(child ParseResult) {
	if !b.hasOffset {
		return
	}
	childOffset, childLength, ok := child.Loc()
	if !ok {
		return
	}
	selfLength := 0
	if b.hasLength {
		selfLength = b.length
	}
	start := b.offset
	if childOffset < start {
		start = childOffset
	}
	end := b.offset + selfLength
	if childEnd := childOffset + childLength; childEnd > end {
		end = childEnd
	}
	// upstream only updates length, keeping the node's own offset
	b.length = end - b.offset
	b.hasLength = true
}

func (b *resultBase) dumpHeader(sb *strings.Builder, kind ResultKind, indent string, isLast bool) {
	sb.WriteString(indent)
	if isLast {
		sb.WriteString("└─")
	} else {
		sb.WriteString("├─")
	}
	sb.WriteString(" ")
	sb.WriteString(kind.String())
	if b.name != "" {
		sb.WriteString(" (")
		sb.WriteString(b.name)
		sb.WriteString(")")
	}
}

func childIndent(indent string, isLast bool) string {
	if isLast {
		return indent + "   "
	}
	return indent + "│  "
}

// Dump renders the parse tree in the same style as upstream's
// ParseResult::ToString.
func Dump(r ParseResult) string {
	var sb strings.Builder
	r.dump(&sb, make(map[*resultBase]bool), "", true)
	return sb.String()
}

// ListResult is a matched sequence. Port of ListParseResult.
type ListResult struct {
	resultBase
	Children []ParseResult
}

func (r *ListResult) Kind() ResultKind { return KindList }

// Child returns the index-th child, panicking on out-of-bounds (the
// transformer indexes by grammar position, so out-of-bounds is a bug).
func (r *ListResult) Child(index int) ParseResult { return r.Children[index] }

func (r *ListResult) dump(sb *strings.Builder, visited map[*resultBase]bool, indent string, isLast bool) {
	dumpChildren(&r.resultBase, KindList, r.Children, sb, visited, indent, isLast)
}

// RepeatResult is one-or-more matches of a repeated element. Port of
// RepeatParseResult.
type RepeatResult struct {
	resultBase
	Children []ParseResult
}

func (r *RepeatResult) Kind() ResultKind { return KindRepeat }

func (r *RepeatResult) dump(sb *strings.Builder, visited map[*resultBase]bool, indent string, isLast bool) {
	dumpChildren(&r.resultBase, KindRepeat, r.Children, sb, visited, indent, isLast)
}

func dumpChildren(b *resultBase, kind ResultKind, children []ParseResult, sb *strings.Builder,
	visited map[*resultBase]bool, indent string, isLast bool) {
	sb.WriteString(indent)
	if isLast {
		sb.WriteString("└─")
	} else {
		sb.WriteString("├─")
	}
	if visited[b] {
		fmt.Fprintf(sb, " %s (%s) [... already printed ...]\n", kindLabel(kind), b.name)
		return
	}
	visited[b] = true
	sb.WriteString(" ")
	sb.WriteString(kind.String())
	if b.name != "" {
		sb.WriteString(" (")
		sb.WriteString(b.name)
		sb.WriteString(")")
	}
	fmt.Fprintf(sb, " [%d children]\n", len(children))
	ci := childIndent(indent, isLast)
	for i, child := range children {
		child.dump(sb, visited, ci, i == len(children)-1)
	}
}

func kindLabel(kind ResultKind) string {
	if kind == KindRepeat {
		return "Repeat"
	}
	return "List"
}

// OptionalResult is a matched `?`. Result is nil when the optional did not
// match. Port of OptionalParseResult.
type OptionalResult struct {
	resultBase
	Result ParseResult
}

func (r *OptionalResult) Kind() ResultKind { return KindOptional }

// HasResult reports whether the optional matched.
func (r *OptionalResult) HasResult() bool { return r.Result != nil }

func (r *OptionalResult) dump(sb *strings.Builder, visited map[*resultBase]bool, indent string, isLast bool) {
	if r.Result != nil {
		// collapse: print the child in place of the optional
		r.Result.dump(sb, visited, indent, isLast)
		return
	}
	sb.WriteString(indent)
	if isLast {
		sb.WriteString("└─")
	} else {
		sb.WriteString("├─")
	}
	sb.WriteString(" OPTIONAL [empty]\n")
}

// ChoiceResult wraps the matched alternative of an ordered choice. Port of
// ChoiceParseResult.
type ChoiceResult struct {
	resultBase
	Result ParseResult
	// Index is the position of the matched alternative.
	Index int
}

func (r *ChoiceResult) Kind() ResultKind { return KindChoice }

func (r *ChoiceResult) dump(sb *strings.Builder, visited map[*resultBase]bool, indent string, isLast bool) {
	sb.WriteString(indent)
	if isLast {
		sb.WriteString("└─")
	} else {
		sb.WriteString("├─")
	}
	fmt.Fprintf(sb, " [CHOICE (idx: %d)] ->\n", r.Index)
	r.Result.dump(sb, visited, childIndent(indent, isLast), true)
}

// IdentifierResult is a matched identifier, unwrapped from quoting. Port of
// IdentifierParseResult.
type IdentifierResult struct {
	resultBase
	Identifier string
}

func (r *IdentifierResult) Kind() ResultKind { return KindIdentifier }

func (r *IdentifierResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindIdentifier, indent, isLast)
	fmt.Fprintf(sb, ": %s\n", r.Identifier)
}

// KeywordResult is a matched keyword or punctuation literal (original
// source spelling). Port of KeywordParseResult.
type KeywordResult struct {
	resultBase
	Keyword string
}

func (r *KeywordResult) Kind() ResultKind { return KindKeyword }

func (r *KeywordResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindKeyword, indent, isLast)
	fmt.Fprintf(sb, ": %q\n", r.Keyword)
}

// NumberResult is a matched number literal (raw text). Port of
// NumberParseResult.
type NumberResult struct {
	resultBase
	Number string
}

func (r *NumberResult) Kind() ResultKind { return KindNumber }

func (r *NumberResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindNumber, indent, isLast)
	fmt.Fprintf(sb, ": %s\n", r.Number)
}

// StringResult is a matched string literal with the quotes and prefix
// stripped and doubled quotes unescaped. Port of StringLiteralParseResult.
type StringResult struct {
	resultBase
	Value      string
	StringKind StringKind
}

func (r *StringResult) Kind() ResultKind { return KindString }

func (r *StringResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindString, indent, isLast)
	fmt.Fprintf(sb, ": '%s'\n", r.Value)
}

// OperatorResult is a matched operator token. Port of OperatorParseResult.
type OperatorResult struct {
	resultBase
	Operator string
}

func (r *OperatorResult) Kind() ResultKind { return KindOperator }

func (r *OperatorResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindOperator, indent, isLast)
	fmt.Fprintf(sb, ": %s\n", r.Operator)
}

// EndOfInputResult marks the consumed end-of-input sentinel. Port of
// EndOfInputParseResult.
type EndOfInputResult struct {
	resultBase
}

func (r *EndOfInputResult) Kind() ResultKind { return KindEndOfInput }

func (r *EndOfInputResult) dump(sb *strings.Builder, _ map[*resultBase]bool, indent string, isLast bool) {
	r.dumpHeader(sb, KindEndOfInput, indent, isLast)
	sb.WriteString("\n")
}

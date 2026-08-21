package matcher

import (
	"strings"

	"github.com/sqlc-dev/darkwing/token"
)

// state carries the token stream and matching position. It plays the role
// of upstream's MatchState + TokenIterator: matchers advance pos on success
// and restore it on failure (upstream achieves the same by matching on a
// copied iterator and propagating the position only on success).
type state struct {
	tokens []token.Token
	pos    int
	// max is the furthest token index reached, the basis for syntax error
	// reporting (upstream: MatchState::max_token_index).
	max     int
	packrat *packratCache
}

func (s *state) current() *token.Token {
	if s.pos < len(s.tokens) {
		return &s.tokens[s.pos]
	}
	return nil
}

func (s *state) advance() {
	s.pos++
	if s.pos > s.max {
		s.max = s.pos
	}
}

// currentOffset returns the byte offset of the current token, mirroring the
// start_offset capture every composite matcher performs.
func (s *state) currentOffset() (int, bool) {
	if t := s.current(); t != nil {
		return t.Span.Start, true
	}
	return 0, false
}

type matcherMeta struct {
	// id is the packrat identity, assigned by the factory (upstream:
	// MatcherAllocator index).
	id int
	// name is the display/result name; set alongside rule.
	name string
	// rule is the grammar rule name attached to this matcher, "" if none.
	rule     string
	hasRule  bool
	memoized bool
}

func (m *matcherMeta) meta() *matcherMeta { return m }

func (m *matcherMeta) setRule(name string) {
	m.rule = name
	m.name = name
	m.hasRule = true
}

// matcher is the interface all matcher-tree nodes implement. Port of the
// duckdb Matcher class hierarchy, restricted to the parse-result path (the
// autocomplete Match/AddSuggestion path is out of scope).
type matcher interface {
	// matchInternal attempts to match at the current position. It returns
	// nil on failure and must leave state.pos unchanged in that case.
	matchInternal(s *state) ParseResult
	meta() *matcherMeta
	String() string
}

// match is the memoization- and rule-attaching wrapper around
// matchInternal. Port of Matcher::MatchParseResult.
func match(m matcher, s *state) ParseResult {
	var r ParseResult
	mm := m.meta()
	if s.packrat != nil && mm.memoized {
		r = s.packrat.match(m, s)
	} else {
		r = m.matchInternal(s)
	}
	if r != nil && mm.hasRule {
		r.base().name = mm.rule
	}
	return r
}

// ciEquals is StringUtil::CIEquals: ASCII case-insensitive equality.
func ciEquals(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

//===----------------------------------------------------------------------===//
// Composite matchers
//===----------------------------------------------------------------------===//

// listMatcher matches a sequence of child matchers. Port of ListMatcher.
type listMatcher struct {
	matcherMeta
	children []matcher
}

func (m *listMatcher) matchInternal(s *state) ParseResult {
	start := s.pos
	offset, hasOffset := s.currentOffset()
	results := make([]ParseResult, 0, len(m.children))
	for _, child := range m.children {
		r := match(child, s)
		if r == nil {
			s.pos = start
			return nil
		}
		results = append(results, r)
	}
	// an empty name implies a subrule, e.g. the bracket group in
	// 'SET' (StandardAssignment / SetTimeZone)
	lr := &ListResult{Children: results}
	lr.name = m.name
	lr.offset, lr.hasOffset = offset, hasOffset
	for _, child := range results {
		lr.encloseChild(child)
	}
	return lr
}

func (m *listMatcher) String() string {
	parts := make([]string, len(m.children))
	for i, child := range m.children {
		parts[i] = name(child)
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// choiceMatcher matches the first succeeding alternative. Port of
// ChoiceMatcher.
type choiceMatcher struct {
	matcherMeta
	children []matcher
}

func (m *choiceMatcher) matchInternal(s *state) ParseResult {
	offset, hasOffset := s.currentOffset()
	for i, child := range m.children {
		r := match(child, s)
		if r != nil {
			cr := &ChoiceResult{Result: r, Index: i}
			cr.name = r.Name()
			cr.offset, cr.hasOffset = offset, hasOffset
			cr.encloseChild(r)
			return cr
		}
	}
	return nil
}

func (m *choiceMatcher) String() string {
	parts := make([]string, len(m.children))
	for i, child := range m.children {
		parts[i] = name(child)
	}
	return strings.Join(parts, " / ")
}

// optionalMatcher matches zero-or-one of its child. Port of
// OptionalMatcher.
type optionalMatcher struct {
	matcherMeta
	child matcher
}

func (m *optionalMatcher) matchInternal(s *state) ParseResult {
	offset, hasOffset := s.currentOffset()
	r := match(m.child, s)
	if r == nil {
		return &OptionalResult{}
	}
	or := &OptionalResult{Result: r}
	or.name = r.Name()
	or.offset, or.hasOffset = offset, hasOffset
	or.encloseChild(r)
	return or
}

func (m *optionalMatcher) String() string {
	return name(m.child) + "?"
}

// repeatMatcher matches one-or-more of its element (`*` compiles to
// Optional(Repeat(...))). Port of RepeatMatcher.
type repeatMatcher struct {
	matcherMeta
	element matcher
}

func (m *repeatMatcher) matchInternal(s *state) ParseResult {
	offset, hasOffset := s.currentOffset()
	first := match(m.element, s)
	if first == nil {
		return nil
	}
	results := []ParseResult{first}
	for {
		r := match(m.element, s)
		if r == nil {
			break
		}
		results = append(results, r)
	}
	rr := &RepeatResult{Children: results}
	rr.offset, rr.hasOffset = offset, hasOffset
	for _, child := range results {
		rr.encloseChild(child)
	}
	return rr
}

func (m *repeatMatcher) String() string {
	return name(m.element) + "*"
}

func name(m matcher) string {
	if mm := m.meta(); mm.name != "" {
		return mm.name
	}
	return m.String()
}

//===----------------------------------------------------------------------===//
// Leaf matchers
//===----------------------------------------------------------------------===//

// keywordMatcher matches one keyword or punctuation literal
// case-insensitively. Port of KeywordMatcher.
type keywordMatcher struct {
	matcherMeta
	keyword string
}

func (m *keywordMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil || !ciEquals(m.keyword, tok.Text) {
		return nil
	}
	offset := tok.Span.Start
	length := len(tok.Text)
	text := tok.Text
	s.advance()
	kr := &KeywordResult{Keyword: text}
	kr.name = m.name
	kr.offset, kr.hasOffset = offset, true
	kr.length, kr.hasLength = length, true
	return kr
}

func (m *keywordMatcher) String() string {
	return "'" + m.keyword + "'"
}

// endOfInputMatcher consumes the EndOfInput sentinel. Port of
// EndOfInputMatcher.
type endOfInputMatcher struct {
	matcherMeta
}

func (m *endOfInputMatcher) matchInternal(s *state) ParseResult {
	tok := s.current()
	if tok == nil || tok.Kind != token.EndOfInput {
		return nil
	}
	s.advance()
	return &EndOfInputResult{}
}

func (m *endOfInputMatcher) String() string {
	return "EndOfInput"
}

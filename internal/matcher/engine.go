package matcher

import (
	"fmt"
	"sync"

	"github.com/sqlc-dev/darkwing/internal/grammar"
	"github.com/sqlc-dev/darkwing/token"
)

// SyntaxError reports a failed match: the furthest token the matcher
// reached, upstream's "syntax error at or near" basis.
type SyntaxError struct {
	// Token is the token at or near the failure (never the end-of-input
	// sentinel when a real token precedes it).
	Token token.Token
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("syntax error at or near %q", e.Token.Text)
}

// MatchError is a non-positional matching error, mirroring the
// ParserException upstream throws from inside a matcher (currently only
// the double-exponent number literal case).
type MatchError struct {
	Msg string
}

func (e *MatchError) Error() string {
	return e.Msg
}

// Engine is an immutable compiled grammar, safe for concurrent use. Each
// Match call gets its own packrat cache, as upstream does per top-level
// statement.
type Engine struct {
	root     matcher
	topLevel matcher
}

// New compiles g starting from rootRule. Rules unreachable from rootRule
// are not compiled (matching upstream's lazy CreateMatcher recursion); the
// standard rule overrides and packrat memoization list are always
// installed first.
func New(g *grammar.Grammar, rootRule string) (*Engine, error) {
	f := newFactory(g)
	for _, name := range packratMemoizedRules {
		f.memoized[name] = true
	}
	f.installOverrides()
	root, err := f.createMatcher(rootRule, nil)
	if err != nil {
		return nil, err
	}
	e := &Engine{root: root}
	// TopLevelStatement is built as a sub-rule of Program; expose it for
	// the statement-at-a-time parse loop when present
	if tls, ok := f.matchers["TopLevelStatement"]; ok {
		e.topLevel = tls
	}
	return e, nil
}

var (
	defaultOnce   sync.Once
	defaultEngine *Engine
	defaultErr    error
)

// Default compiles the embedded DuckDB grammar once, rooted at Program,
// and returns the shared immutable engine.
func Default() (*Engine, error) {
	defaultOnce.Do(func() {
		g, err := grammar.Load()
		if err != nil {
			defaultErr = err
			return
		}
		e, err := New(g, "Program")
		if err != nil {
			defaultErr = err
			return
		}
		if e.topLevel == nil {
			defaultErr = fmt.Errorf("grammar is missing required root rule 'TopLevelStatement'")
			return
		}
		defaultEngine = e
	})
	return defaultEngine, defaultErr
}

// Match matches the root rule against tokens starting at pos. On success
// it returns the parse result and the position after the consumed tokens.
// On failure it returns a *SyntaxError locating the furthest token
// reached. Port of PEGTransformerFactory::TransformTopLevelStatement's
// match-and-report step.
func (e *Engine) Match(tokens []token.Token, pos int) (ParseResult, int, error) {
	return runMatch(e.root, tokens, pos)
}

// MatchTopLevel matches a single TopLevelStatement, the entry point of the
// statement-at-a-time parse loop.
func (e *Engine) MatchTopLevel(tokens []token.Token, pos int) (ParseResult, int, error) {
	if e.topLevel == nil {
		return nil, pos, fmt.Errorf("engine has no TopLevelStatement rule")
	}
	return runMatch(e.topLevel, tokens, pos)
}

func runMatch(root matcher, tokens []token.Token, pos int) (result ParseResult, newPos int, err error) {
	s := &state{
		tokens:  tokens,
		pos:     pos,
		max:     pos,
		packrat: newPackratCache(),
	}
	defer func() {
		if r := recover(); r != nil {
			if me, ok := r.(*MatchError); ok {
				result, newPos, err = nil, pos, me
				return
			}
			panic(r)
		}
	}()
	r := match(root, s)
	if r == nil {
		return nil, pos, syntaxError(tokens, s.max)
	}
	return r, s.pos, nil
}

// syntaxError builds the furthest-failure error, walking back past the
// end-of-input sentinel so the message names a real token.
func syntaxError(tokens []token.Token, maxTokenIndex int) error {
	if len(tokens) == 0 {
		return &SyntaxError{Token: token.Token{Kind: token.EndOfInput}}
	}
	idx := maxTokenIndex
	if idx >= len(tokens) {
		idx = len(tokens) - 1
	}
	if idx > 0 && tokens[idx].Kind == token.EndOfInput {
		idx--
	}
	if idx < 0 {
		idx = 0
	}
	return &SyntaxError{Token: tokens[idx]}
}

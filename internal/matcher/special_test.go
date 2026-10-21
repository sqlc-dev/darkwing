package matcher

import (
	"errors"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/grammar"
)

// collect returns every node of a parse tree that pred selects, in
// pre-order.
func collect(r ParseResult, pred func(ParseResult) bool) []ParseResult {
	var out []ParseResult
	var walk func(ParseResult)
	walk = func(n ParseResult) {
		if pred(n) {
			out = append(out, n)
		}
		switch n := n.(type) {
		case *ListResult:
			for _, c := range n.Children {
				walk(c)
			}
		case *RepeatResult:
			for _, c := range n.Children {
				walk(c)
			}
		case *ChoiceResult:
			walk(n.Result)
		case *OptionalResult:
			if n.Result != nil {
				walk(n.Result)
			}
		}
	}
	walk(r)
	return out
}

func stringResults(t *testing.T, engine *Engine, sql string) []*StringResult {
	t.Helper()
	r, _, err := engine.MatchTopLevel(mustTokens(t, sql), 0)
	if err != nil {
		t.Fatalf("%q: %v", sql, err)
	}
	var out []*StringResult
	for _, n := range collect(r, func(n ParseResult) bool { _, ok := n.(*StringResult); return ok }) {
		out = append(out, n.(*StringResult))
	}
	return out
}

func identifiers(t *testing.T, engine *Engine, sql string) []*IdentifierResult {
	t.Helper()
	r, _, err := engine.MatchTopLevel(mustTokens(t, sql), 0)
	if err != nil {
		t.Fatalf("%q: %v", sql, err)
	}
	var out []*IdentifierResult
	for _, n := range collect(r, func(n ParseResult) bool { _, ok := n.(*IdentifierResult); return ok }) {
		out = append(out, n.(*IdentifierResult))
	}
	return out
}

func rejectNear(t *testing.T, engine *Engine, sql, near string) {
	t.Helper()
	_, err := matchAll(t, engine, sql)
	var syntaxErr *SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("reject %q: err = %v, want *SyntaxError", sql, err)
		return
	}
	if syntaxErr.Token.Text != near {
		t.Errorf("reject %q: at or near %q, want %q", sql, syntaxErr.Token.Text, near)
	}
}

// Backtick-quoted identifiers unwrap like double-quoted ones, each
// un-doubling its own delimiter (IdentifierMatcher::UnquoteIdentifier).
func TestBacktickIdentifiers(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	ids := identifiers(t, engine, "SELECT `a``b` AS `x\"y` FROM `select`.\"t\"\"u\"")
	var got []string
	for _, id := range ids {
		got = append(got, id.Identifier)
	}
	want := []string{"a`b", `x"y`, "select", `t"u`}
	if len(got) != len(want) {
		t.Fatalf("identifiers = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("identifier %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A prefixed string literal (x'41', e'ab') is never an identifier, not
// even in a table-name position that accepts '...' paths.
func TestPrefixedStringIsNotIdentifier(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rejectNear(t, engine, "SELECT * FROM x'41'", "x'41'")
	rejectNear(t, engine, "SELECT 1 e'ab'", "e'ab'")
	// the plain path literal is still accepted
	ids := identifiers(t, engine, "SELECT * FROM 'data.csv'")
	if len(ids) != 1 || ids[0].Identifier != "data.csv" {
		t.Errorf("path literal = %v", ids)
	}
}

func TestStringLiterals(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql    string
		value  string
		kind   StringKind
		offset int
		length int
	}{
		{"SELECT 'it''s'", "it's", StringStandard, 7, 7},
		// dollar-quoted: raw contents, no quote doubling, true source span
		{"SELECT $$it's$$", "it's", StringStandard, 7, 8},
		{"SELECT $q$a$$b$q$", "a$$b", StringStandard, 7, 10},
		// a literal continues across newlines, its span covering every part
		{"SELECT 'a'\n'b'\n  'c'", "abc", StringStandard, 7, 13},
		// a prefixed literal can be continued, a continuation cannot be
		// prefixed
		{"SELECT E'a'\n'b'", "ab", StringEscape, 7, 8},
		{"SELECT X'4'\n'1'", "41", StringHexadecimal, 7, 8},
	} {
		got := stringResults(t, engine, tc.sql)
		if len(got) != 1 {
			t.Errorf("%q: %d string literals, want 1", tc.sql, len(got))
			continue
		}
		s := got[0]
		off, length, _ := s.Loc()
		if s.Value != tc.value || s.StringKind != tc.kind || off != tc.offset || length != tc.length {
			t.Errorf("%q = %q kind %d at (%d, %d), want %q kind %d at (%d, %d)",
				tc.sql, s.Value, s.StringKind, off, length, tc.value, tc.kind, tc.offset, tc.length)
		}
	}
	// no continuation on the same line, after a block comment, for a
	// prefixed literal, or from a dollar-quoted literal
	rejectNear(t, engine, "SELECT 'a' 'b'", "'b'")
	rejectNear(t, engine, "SELECT 'a'\n/* c */ 'b'", "'b'")
	rejectNear(t, engine, "SELECT 'a'\nE'b'", "E'b'")
	rejectNear(t, engine, "SELECT $$a$$\n'b'", "'b'")
	// a line comment is trivia like any other: the newline it ends with
	// still separates the parts
	if got := stringResults(t, engine, "SELECT 'a' -- c\n'b'"); len(got) != 1 || got[0].Value != "ab" {
		t.Errorf("continuation across a line comment = %v", got)
	}
}

// A number has at most one dot, none in its exponent.
func TestNumberLiteralDots(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rejectNear(t, engine, "SELECT 1.2.3", "1.2.3")
	rejectNear(t, engine, "SELECT .5.", ".5.")
	rejectNear(t, engine, "SELECT 1e5.5", "1e5.5")
	for _, sql := range []string{"SELECT 1.5", "SELECT .5", "SELECT 1.", "SELECT 1.5e-3"} {
		if _, err := matchAll(t, engine, sql); err != nil {
			t.Errorf("accept %q: %v", sql, err)
		}
	}
}

// OperatorLiteral skips the operators with a precedence level of their own
// (OperatorMatcher::HasSpecialPrecedence) but, unlike before v2.0.0, takes
// single-character operators; AnyOperatorLiteral takes every operator.
func TestOperatorMatcherModes(t *testing.T) {
	g, err := grammar.Parse("Root <- Generic / ('ANY' Any)\nGeneric <- OperatorLiteral EndOfInput\nAny <- AnyOperatorLiteral EndOfInput\nOperatorLiteral <- 'x'\nAnyOperatorLiteral <- 'x'\n")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(g, "Root")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		op           string
		generic, any bool
	}{
		{"+", true, true},
		{"-", true, true},
		{"%", true, true},
		{"->>", true, true},
		{"@>", true, true},
		{"||", true, true},
		{"=", false, true},
		{"<", false, true},
		{"~", false, true},
		{"->", false, true},
		{"<=", false, true},
		{"<>", false, true},
		{"!~~*", false, true},
		// not operator characters at all
		{"::", false, false},
		{"#", false, false},
	} {
		if _, _, err := engine.Match(mustTokens(t, tc.op), 0); (err == nil) != tc.generic {
			t.Errorf("OperatorLiteral %q: err = %v, want match %v", tc.op, err, tc.generic)
		}
		if _, _, err := engine.Match(mustTokens(t, "ANY "+tc.op), 0); (err == nil) != tc.any {
			t.Errorf("AnyOperatorLiteral %q: err = %v, want match %v", tc.op, err, tc.any)
		}
	}
}

// A column alias without AS must be a non-keyword or a bare-label keyword.
func TestBareColLabel(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT 1 x", "SELECT 1 abort", "SELECT 1 AS year", "SELECT 1 \"year\""} {
		if _, err := matchAll(t, engine, sql); err != nil {
			t.Errorf("accept %q: %v", sql, err)
		}
	}
	rejectNear(t, engine, "SELECT 1 year", "year")
}

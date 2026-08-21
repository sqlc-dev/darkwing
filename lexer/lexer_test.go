package lexer

import (
	"errors"
	"testing"

	"github.com/sqlc-dev/darkwing/token"
)

type tok struct {
	kind  token.Kind
	text  string
	start int
}

func check(t *testing.T, sql string, want []tok) {
	t.Helper()
	got, err := Tokenize(sql)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", sql, err)
	}
	// every token stream ends with the EndOfInput sentinel
	if len(got) == 0 || got[len(got)-1].Kind != token.EndOfInput {
		t.Fatalf("Tokenize(%q): missing EndOfInput sentinel: %v", sql, got)
	}
	if got[len(got)-1].Span.Start != len(sql) {
		t.Errorf("Tokenize(%q): sentinel offset = %d, want %d", sql, got[len(got)-1].Span.Start, len(sql))
	}
	body := got[:len(got)-1]
	if len(body) != len(want) {
		t.Fatalf("Tokenize(%q) = %v, want %v", sql, body, want)
	}
	for i, w := range want {
		g := body[i]
		if g.Kind != w.kind || g.Text != w.text || g.Span.Start != w.start {
			t.Errorf("Tokenize(%q)[%d] = %v, want %s(%q)@%d", sql, i, g, w.kind, w.text, w.start)
		}
	}
}

func checkErr(t *testing.T, sql, msg string, offset int) {
	t.Helper()
	_, err := Tokenize(sql)
	var lexErr *Error
	if !errors.As(err, &lexErr) {
		t.Fatalf("Tokenize(%q): err = %v, want *Error", sql, err)
	}
	if lexErr.Msg != msg || lexErr.Offset != offset {
		t.Errorf("Tokenize(%q): err = %q@%d, want %q@%d", sql, lexErr.Msg, lexErr.Offset, msg, offset)
	}
}

func TestKeywordsAndIdentifiers(t *testing.T) {
	check(t, "SELECT a FROM t", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Identifier, "a", 7},
		{token.Keyword, "FROM", 9},
		{token.Identifier, "t", 14},
	})
	// keywords are case-insensitive; the raw spelling is preserved
	check(t, "select SeLeCt", []tok{
		{token.Keyword, "select", 0},
		{token.Keyword, "SeLeCt", 7},
	})
	// '$' is a valid non-initial identifier character
	check(t, "a$b", []tok{{token.Identifier, "a$b", 0}})
}

func TestTerminator(t *testing.T) {
	check(t, "SELECT 1; SELECT 2", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1", 7},
		{token.Terminator, ";", 8},
		{token.Keyword, "SELECT", 10},
		{token.Number, "2", 17},
	})
	check(t, ";;", []tok{
		{token.Terminator, ";", 0},
		{token.Terminator, ";", 1},
	})
}

func TestStrings(t *testing.T) {
	check(t, "'hello'", []tok{{token.String, "'hello'", 0}})
	// doubled-quote escape stays inside one token
	check(t, "'it''s'", []tok{{token.String, "'it''s'", 0}})
	// escape strings: backslash escapes the quote only in E'...'
	check(t, `E'a\'b'`, []tok{{token.String, `E'a\'b'`, 0}})
	check(t, "e'x'", []tok{{token.String, "e'x'", 0}})
	// other special string prefixes
	check(t, "N'x' X'ff' B'01'", []tok{
		{token.String, "N'x'", 0},
		{token.String, "X'ff'", 5},
		{token.String, "B'01'", 11},
	})
	// prefix letter not followed by a quote is an ordinary identifier
	check(t, "Ex 'y'", []tok{
		{token.Identifier, "Ex", 0},
		{token.String, "'y'", 3},
	})
}

func TestQuotedIdentifiers(t *testing.T) {
	check(t, `"my table"`, []tok{{token.Identifier, `"my table"`, 0}})
	check(t, `"a""b"`, []tok{{token.Identifier, `"a""b"`, 0}})
}

func TestDollarQuotedStrings(t *testing.T) {
	// empty tag: rewritten to a plain quoted literal
	check(t, "$$abc$$", []tok{{token.String, "'abc'", 0}})
	// named tag
	check(t, "$tag$abc$tag$", []tok{{token.String, "'abc'", 0}})
	// embedded quotes are doubled during the rewrite
	check(t, "$$it's$$", []tok{{token.String, "'it''s'", 0}})
	// mismatched inner marker does not close the string
	check(t, "$a$x$b$y$a$", []tok{{token.String, "'x$b$y'", 0}})
	// $1 is a parameter, never a dollar-quote opener
	check(t, "$1", []tok{
		{token.Operator, "$", 0},
		{token.Number, "1", 1},
	})
	// $tag with no closing dollar is a collabel parameter ("name" itself
	// is an unreserved keyword, so use a non-keyword tag)
	check(t, "$xyz", []tok{
		{token.Operator, "$", 0},
		{token.Identifier, "xyz", 1},
	})
	// an invalid tag character stops the marker scan: parameter again
	check(t, "$na-me$x$na-me$", []tok{
		{token.Operator, "$", 0},
		{token.Identifier, "na", 1},
		{token.Operator, "-", 3},
		{token.Identifier, "me$x$na", 4},
		{token.Operator, "-", 11},
		{token.Identifier, "me$", 12},
	})
	// unterminated dollar-quote: raw text token flagged unterminated
	got, err := Tokenize("$$abc")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "$$abc" || !got[0].Unterminated || got[0].Kind != token.String {
		t.Errorf("unterminated dollar quote = %v", got[0])
	}
}

func TestParameters(t *testing.T) {
	check(t, "? $1 $abc", []tok{
		{token.Operator, "?", 0},
		{token.Operator, "$", 2},
		{token.Number, "1", 3},
		{token.Operator, "$", 5},
		{token.Identifier, "abc", 6},
	})
	// trailing lone '$' at end of input becomes an identifier-typed token
	// (upstream's last-token handling in the STANDARD state)
	check(t, "SELECT $", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Identifier, "$", 7},
	})
}

func TestNumbers(t *testing.T) {
	check(t, "1 1.5 .5 1e5 1.5e-3 1E+2", []tok{
		{token.Number, "1", 0},
		{token.Number, "1.5", 2},
		{token.Number, ".5", 6},
		{token.Number, "1e5", 9},
		{token.Number, "1.5e-3", 13},
		{token.Number, "1E+2", 20},
	})
	// hex literals lex as number then identifier-ish keyword state:
	// 0x1A: '0' begins numeric; 'x' is not a number char -> backtrack
	check(t, "0x1A", []tok{
		{token.Number, "0", 0},
		{token.Identifier, "x1A", 1},
	})
	// underscore separators: only underscore-digit is allowed
	check(t, "1_000", []tok{{token.Number, "1_000", 0}})
	check(t, "1__0", []tok{
		{token.Number, "1", 0},
		{token.Identifier, "__0", 1},
	})
	// double exponent splits at the second marker
	check(t, "1ee5", []tok{
		{token.Number, "1", 0},
		{token.Identifier, "ee5", 1},
	})
	// '+' only continues the number directly after the exponent marker
	check(t, "1+5", []tok{
		{token.Number, "1", 0},
		{token.Operator, "+", 1},
		{token.Number, "5", 2},
	})
	// ".e100" is not a number: no digit before the marker
	check(t, ".e100", []tok{
		{token.Number, ".", 0},
		{token.Identifier, "e100", 1},
	})
}

func TestOperators(t *testing.T) {
	check(t, "a::b", []tok{
		{token.Identifier, "a", 0},
		{token.Operator, "::", 1},
		{token.Identifier, "b", 3},
	})
	check(t, "j->'k' j->>'k'", []tok{
		{token.Identifier, "j", 0},
		{token.Operator, "->", 1},
		{token.String, "'k'", 3},
		{token.Identifier, "j", 7},
		{token.Operator, "->>", 8},
		{token.String, "'k'", 11},
	})
	check(t, "2**3 7//2 x:=1", []tok{
		{token.Number, "2", 0},
		{token.Operator, "**", 1},
		{token.Number, "3", 3},
		{token.Number, "7", 5},
		{token.Operator, "//", 6},
		{token.Number, "2", 8},
		{token.Identifier, "x", 10},
		{token.Operator, ":=", 11},
		{token.Number, "1", 13},
	})
	// multi-character operator tokens
	check(t, "a >= b <> c", []tok{
		{token.Identifier, "a", 0},
		{token.Operator, ">=", 2},
		{token.Identifier, "b", 5},
		{token.Operator, "<>", 7},
		{token.Identifier, "c", 10},
	})
	// single-byte operators are individual tokens
	check(t, "(a, b)", []tok{
		{token.Operator, "(", 0},
		{token.Identifier, "a", 1},
		{token.Operator, ",", 2},
		{token.Identifier, "b", 4},
		{token.Operator, ")", 5},
	})
	// PostgreSQL trimming rule: an operator cannot end in '+' unless it
	// contains one of ~ ! @ # % ^ & | ` ?
	check(t, "a=+b", []tok{
		{token.Identifier, "a", 0},
		{token.Operator, "=", 1},
		{token.Operator, "+", 2},
		{token.Identifier, "b", 3},
	})
	check(t, "a@+b", []tok{
		{token.Identifier, "a", 0},
		{token.Operator, "@+", 1},
		{token.Identifier, "b", 3},
	})
	// '!=' stays one operator token
	check(t, "a != b", []tok{
		{token.Identifier, "a", 0},
		{token.Operator, "!=", 2},
		{token.Identifier, "b", 5},
	})
}

func TestComments(t *testing.T) {
	// comments are filtered out entirely
	check(t, "SELECT 1 -- trailing", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1", 7},
	})
	check(t, "-- line\nSELECT 1", []tok{
		{token.Keyword, "SELECT", 8},
		{token.Number, "1", 15},
	})
	check(t, "SELECT /* mid */ 1", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1", 17},
	})
	// block comments nest
	check(t, "SELECT /* a /* b */ c */ 1", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1", 25},
	})
	// unterminated comments are not an error: the rest is a comment
	check(t, "SELECT 1 /* open", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1", 7},
	})
}

func TestErrors(t *testing.T) {
	checkErr(t, "SELECT 'abc", "unterminated string literal", 7)
	checkErr(t, `SELECT "abc`, "unterminated quoted identifier", 7)
	checkErr(t, `SELECT ""`, "zero-length delimited identifier", 7)
}

func TestLastTokenStates(t *testing.T) {
	// number at end of input is pushed without backtracking
	check(t, "SELECT 1e", []tok{
		{token.Keyword, "SELECT", 0},
		{token.Number, "1e", 7},
	})
	// operator at end of input
	check(t, "1 =", []tok{
		{token.Number, "1", 0},
		{token.Operator, "=", 2},
	})
	// keyword state at end resolves against the keyword table
	check(t, "SELECT", []tok{{token.Keyword, "SELECT", 0}})
	check(t, "selectx", []tok{{token.Identifier, "selectx", 0}})
}

func TestEmptyInput(t *testing.T) {
	got, err := Tokenize("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != token.EndOfInput {
		t.Errorf("Tokenize(\"\") = %v, want only the sentinel", got)
	}
}

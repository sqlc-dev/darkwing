// Error message and position fidelity (milestone 6): darkwing's rendered
// parse errors must match the pinned DuckDB build's. The corpus records
// the first line of the oracle's message for every must-reject case;
// TestCorpusErrorMessages holds darkwing to it exactly. Positions are not
// recorded in the corpus (the CLI renders them as a LINE/caret display
// below the first line), so TestErrorPositions pins them with hand-written
// cases instead.
package parser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/testfile"
)

// rejectError parses sql and returns the *Error it must fail with.
func rejectError(t *testing.T, sql string) *Error {
	t.Helper()
	_, err := Parse(context.Background(), strings.NewReader(sql))
	if err == nil {
		t.Fatalf("Parse(%q): expected error", sql)
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("Parse(%q): got %T (%v), want *parser.Error", sql, err, err)
	}
	return pe
}

// TestCorpusErrorMessages is the negative-corpus message gate: for every
// must-reject case, darkwing's error must render the oracle's recorded
// first line verbatim.
func TestCorpusErrorMessages(t *testing.T) {
	for _, path := range corpusFiles(t) {
		rel := strings.TrimPrefix(path, "testdata/")
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			file, err := testfile.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := testfile.ReadMetadata(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := range file.Cases {
				c := &file.Cases[i]
				key := c.Key()
				if !c.Reject {
					continue
				}
				if _, skip := meta.Skip[key]; skip {
					continue
				}
				if _, todo := meta.Todo[key]; todo {
					continue
				}
				got := classify(nil, c.SQL)
				if !got.reject {
					// TestCorpus reports accept/reject disagreements
					continue
				}
				// the oracle records only the first line; darkwing's
				// multi-line messages (e.g. the PIVOT hints) match beyond it
				firstLine, _, _ := strings.Cut(got.detail, "\n")
				if firstLine != c.Error {
					t.Errorf("case %s: error message diverges from the oracle\nSQL:  %s\nwant: %s\ngot:  %s",
						key, c.SQL, c.Error, firstLine)
				}
			}
		})
	}
}

// TestErrorPositions pins Offset/Line/Column on representative failures.
// Column counts bytes from 1, as the pinned CLI's caret display does.
func TestErrorPositions(t *testing.T) {
	tests := []struct {
		sql       string
		msg       string // rendered first line
		offset    int
		line, col int
	}{
		{
			// v2.0.0 dropped bare expression statements from the
			// library grammar, so the failure lands on SELCT itself
			sql:    "SELCT 1",
			msg:    `Parser Error: syntax error at or near "SELCT"`,
			offset: 0, line: 1, col: 1,
		},
		{
			sql:    "SELECT 1 2",
			msg:    `Parser Error: syntax error at or near "2"`,
			offset: 9, line: 1, col: 10,
		},
		{
			sql:    "SELECT 1;\nSELCT 2;",
			msg:    `Parser Error: syntax error at or near "SELCT"`,
			offset: 10, line: 2, col: 1,
		},
		{
			sql:    "SELECT 1\nFROM t\nWHERE (",
			msg:    `Parser Error: syntax error at or near "("`,
			offset: 22, line: 3, col: 7,
		},
		{
			// whole-query UTF-8 validation, positioned at the bad byte
			sql:    "SELECT\n  42\nFR\xffOM t;",
			msg:    "Parser Error: Invalid UTF-8 in query (byte sequence mismatch)",
			offset: 14, line: 3, col: 3,
		},
		{
			// a well-formed sequence encoding a surrogate is the other
			// UnicodeInvalidReason
			sql:    "SELECT '\xed\xa0\x80'",
			msg:    "Parser Error: Invalid UTF-8 in query (invalid unicode)",
			offset: 8, line: 1, col: 9,
		},
	}
	for _, tt := range tests {
		pe := rejectError(t, tt.sql)
		firstLine, _, _ := strings.Cut(pe.Error(), "\n")
		if firstLine != tt.msg {
			t.Errorf("Parse(%q): message = %q, want %q", tt.sql, firstLine, tt.msg)
		}
		if pe.Offset != tt.offset || pe.Line != tt.line || pe.Column != tt.col {
			t.Errorf("Parse(%q): position = offset %d line %d col %d, want offset %d line %d col %d",
				tt.sql, pe.Offset, pe.Line, pe.Column, tt.offset, tt.line, tt.col)
		}
	}
}

// TestErrorMessages pins messages that only fire on inputs the corpus
// does not cover.
func TestErrorMessages(t *testing.T) {
	tests := []struct{ sql, msg string }{
		// escape-literal UTF-8 classification: raw invalid byte vs
		// well-formed surrogate sequence
		{`SELECT e'\xFF'`, "Parser Error: Invalid UTF-8 in escape string literal at byte offset 0: byte mismatch"},
		{`SELECT e'a\xed\xa0\x80'`, "Parser Error: Invalid UTF-8 in escape string literal at byte offset 1: invalid unicode"},
		// KeywordHelper::WriteOptionallyQuoted quotes keywords and
		// non-plain identifiers, and leaves plain ones bare
		{"CREATE TABLE t (foo)", "Parser Error: Column foo must have a type or be defined as a GENERATED column."},
		{"CREATE TABLE t (name)", `Parser Error: Column "name" must have a type or be defined as a GENERATED column.`},
		{`CREATE TABLE t ("A b")`, `Parser Error: Column "A b" must have a type or be defined as a GENERATED column.`},
		// upstream names the last offending RESET option
		{"ALTER TABLE t RESET (a='1', b='2')", `Parser Error: Reset option "b" cannot set any value. Did you mean to use SET?`},
		{"ALTER TABLE t RESET (a='1', b)", `Parser Error: Reset option "a" cannot set any value. Did you mean to use SET?`},
		// a deeper operand error beats the chained-comparison ban
		{"SELECT 1 < 2 < [3][]", "Parser Error: Empty subscript '[]' is not allowed"},
		{"SELECT 1 < 2 < 3", "Parser Error: Chained comparisons are not supported, use AND to combine comparisons"},
		// the offending token's raw spelling goes between plain quotes
		// with no escaping (found by cmd/difftest)
		{`TABLE ALTER "other name" RENAME`, `Parser Error: syntax error at or near ""other name""`},
		{`SELECT E'\\' = '\' '\';`, `Parser Error: syntax error at or near "'\'"`},
	}
	for _, tt := range tests {
		pe := rejectError(t, tt.sql)
		firstLine, _, _ := strings.Cut(pe.Error(), "\n")
		if firstLine != tt.msg {
			t.Errorf("Parse(%q): message = %q, want %q", tt.sql, firstLine, tt.msg)
		}
	}
}

// TestAnalyzeUTF8 pins the Utf8Proc::Analyze port's classification and
// positions, including the deliberate divergence from the standard
// library: overlong encodings pass.
func TestAnalyzeUTF8(t *testing.T) {
	tests := []struct {
		in     string
		reason utf8Reason
		pos    int
	}{
		{"", utf8Valid, -1},
		{"plain ascii", utf8Valid, -1},
		{"caf\xc3\xa9", utf8Valid, -1},
		{"\xf0\x9f\xa6\x86", utf8Valid, -1}, // U+1F986 (duck)
		{"\xff", utf8ByteMismatch, 0},
		{"a\x80", utf8ByteMismatch, 1},               // stray continuation byte
		{"a\xc3", utf8ByteMismatch, 1},               // truncated 2-byte sequence
		{"ab\xe0\xa0", utf8ByteMismatch, 2},          // truncated 3-byte sequence
		{"\xc3(", utf8ByteMismatch, 0},               // bad continuation byte
		{"\xed\xa0\x80", utf8InvalidUnicode, 0},      // surrogate U+D800
		{"a\xf4\x90\x80\x80", utf8InvalidUnicode, 1}, // > U+10FFFF
		{"\xc0\xaf", utf8Valid, -1},                  // overlong '/', accepted like upstream
	}
	for _, tt := range tests {
		reason, pos := analyzeUTF8(tt.in)
		if reason != tt.reason || pos != tt.pos {
			t.Errorf("analyzeUTF8(%q) = (%v, %d), want (%v, %d)", tt.in, reason, pos, tt.reason, tt.pos)
		}
	}
}

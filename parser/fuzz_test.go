// Milestone 6 hardening: native fuzzing of the public Parse entry point
// and a shared-engine parallel parse test (run under -race by the dev
// loop and CI).
package parser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sqlc-dev/darkwing/ast"
)

// checkParseInvariants parses src and enforces what Parse promises for
// any input: no panic other than the controlled internal kinds, errors
// are *parser.Error with sane positions, and on success the statement
// spans tile the input.
func checkParseInvariants(t *testing.T, src string) {
	t.Helper()
	stmts, err := Parse(context.Background(), strings.NewReader(src))
	if err != nil {
		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("Parse(%q): error is %T (%v), want *parser.Error", src, err, err)
		}
		if pe.Offset < -1 || pe.Offset > len(src) {
			t.Fatalf("Parse(%q): error offset %d out of range", src, pe.Offset)
		}
		if pe.Line < 1 || pe.Column < 1 {
			t.Fatalf("Parse(%q): error position line %d col %d", src, pe.Line, pe.Column)
		}
		return
	}
	// spans tile the input: the first statement starts at 0, each next
	// one starts where the previous ended, the last ends at len(src)
	prev := 0
	for i, stmt := range stmts {
		if stmt == nil {
			t.Fatalf("Parse(%q): statement %d is nil", src, i)
		}
		sp := ast.Span{Start: stmt.Pos(), End: stmt.End()}
		if sp.Start != prev {
			t.Fatalf("Parse(%q): statement %d starts at %d, want %d", src, i, sp.Start, prev)
		}
		if sp.End < sp.Start || sp.End > len(src) {
			t.Fatalf("Parse(%q): statement %d span [%d,%d) out of range", src, i, sp.Start, sp.End)
		}
		prev = sp.End
	}
	if len(stmts) > 0 && prev != len(src) {
		t.Fatalf("Parse(%q): last statement ends at %d, want %d", src, prev, len(src))
	}
}

var fuzzSeeds = []string{
	"",
	";;",
	"SELECT 1",
	"SELECT * FROM t WHERE a = $1 AND b = ? AND c = $name",
	"FROM t SELECT x: a + 1",
	"WITH RECURSIVE c AS (SELECT 1) SELECT * FROM c UNION ALL BY NAME SELECT 2",
	"INSERT OR REPLACE INTO t BY NAME SELECT 1 RETURNING *",
	"CREATE TABLE t (a INT PRIMARY KEY, b STRUCT(x INT[], y MAP(TEXT, UNION(i INT, s TEXT))))",
	"PIVOT sales ON month USING sum(amount)",
	"SELECT [x + 1 FOR x IN [1,2,3] IF x > 1], l[1:3:2], r['k'], s.*.x",
	"SELECT interval '2 10' years to months",
	"SELECT e'\\x41\\n' || $tag$body$tag$ || 'lit''eral'",
	"SELECT a -> '$.b' ->> 'c' FROM j -- comment\n/* block /* nested */ */",
	"ATTACH 'f.db' AS d (READ_ONLY); USE d; SET threads = 4;",
	"SELECT (((((((1",
	"SELCT 1 2 3 (",
	"SELECT '\xff'",
	"select 1 = 1 = 1",
	"[1,2,3][];",
}

func FuzzParse(f *testing.F) {
	for _, seed := range fuzzSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 1<<14 {
			// long adversarial inputs stress the matcher superlinearly;
			// throughput is covered by the benchmarks
			t.Skip()
		}
		checkParseInvariants(t, src)
	})
}

// TestParseInvariantsOnSeeds keeps the fuzz seeds honest in ordinary
// test runs (go test does not execute the fuzz engine).
func TestParseInvariantsOnSeeds(t *testing.T) {
	for _, seed := range fuzzSeeds {
		checkParseInvariants(t, seed)
	}
}

// TestParallelParse hammers the shared engine from many goroutines — the
// race-clean gate for the immutable compiled grammar and the per-call
// packrat state (meaningful under -race, as the dev loop and CI run it).
func TestParallelParse(t *testing.T) {
	inputs := append([]string(nil), fuzzSeeds...)
	inputs = append(inputs, benchComplexSelect, unmatchedParens(19))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				src := inputs[(g+i)%len(inputs)]
				_, err := Parse(context.Background(), strings.NewReader(src))
				var pe *Error
				if err != nil && !errors.As(err, &pe) {
					t.Errorf("Parse(%q): unexpected error type %T", src, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

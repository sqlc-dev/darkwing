// Milestone 6 benchmarks: parse throughput on representative statements
// and upstream's pathological-backtracking case. Upstream's motivating
// packrat example — 19 unmatched '(' going from 10.6 s to 1 ms — is both
// a benchmark and a regression test here.
package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/lexer"
)

const benchSimpleSelect = "SELECT a, b + 1 FROM t WHERE c = 'x' ORDER BY a LIMIT 10"

const benchComplexSelect = `
WITH sales AS (
    SELECT region, product, amount, sold_at
    FROM raw_sales
    WHERE sold_at >= DATE '2024-01-01'
), ranked AS (
    SELECT region, product,
           sum(amount) AS total,
           rank() OVER (PARTITION BY region ORDER BY sum(amount) DESC) AS r
    FROM sales
    GROUP BY ALL
)
SELECT r.region, r.product, r.total,
       t.name AS region_name,
       CASE WHEN r.total > 1000 THEN 'big' ELSE 'small' END AS bucket
FROM ranked r
JOIN regions t ON r.region = t.id
WHERE r.r <= 3
QUALIFY row_number() OVER (ORDER BY r.total DESC) <= 100
ORDER BY ALL`

// benchMixed exercises the non-SELECT transformers.
var benchMixed = []string{
	"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y') ON CONFLICT (a) DO UPDATE SET b = excluded.b RETURNING a",
	"UPDATE t SET b = b + 1 FROM u WHERE t.a = u.a",
	"DELETE FROM t USING u WHERE t.a = u.a RETURNING *",
	"CREATE TABLE IF NOT EXISTS t (a INTEGER PRIMARY KEY, b VARCHAR DEFAULT 'x', c STRUCT(x INT, y MAP(TEXT, DOUBLE[])))",
	"COPY t TO 'out.parquet' (FORMAT parquet, COMPRESSION zstd)",
	"PIVOT sales ON month IN (jan, feb) USING sum(amount) GROUP BY region",
	"SELECT * EXCLUDE (a) REPLACE (b + 1 AS b) FROM t USING SAMPLE 10%",
	"ATTACH 'other.db' AS other (READ_ONLY)",
	"PREPARE q AS SELECT * FROM t WHERE a = $1 AND b = $name",
}

func benchmarkParse(b *testing.B, sql string) {
	b.Helper()
	// prime the engine compile outside the timed loop
	if _, err := Parse(context.Background(), strings.NewReader(sql)); err != nil {
		b.Fatalf("Parse(%q): %v", sql, err)
	}
	b.SetBytes(int64(len(sql)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseString(context.Background(), sql); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseSimpleSelect(b *testing.B)  { benchmarkParse(b, benchSimpleSelect) }
func BenchmarkParseComplexSelect(b *testing.B) { benchmarkParse(b, benchComplexSelect) }

func BenchmarkParseMixed(b *testing.B) {
	all := strings.Join(benchMixed, ";\n") + ";"
	benchmarkParse(b, all)
}

func BenchmarkTokenize(b *testing.B) {
	b.SetBytes(int64(len(benchComplexSelect)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := lexer.Tokenize(benchComplexSelect); err != nil {
			b.Fatal(err)
		}
	}
}

// unmatchedParens is upstream's pathological backtracking input: without
// packrat memoization each unmatched '(' multiplies the work (10.6 s at
// 19 parens in upstream's pre-memoization measurement).
func unmatchedParens(n int) string {
	return "SELECT " + strings.Repeat("(", n) + "1"
}

func BenchmarkUnmatchedParens19(b *testing.B) {
	sql := unmatchedParens(19)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseString(context.Background(), sql); err == nil {
			b.Fatal("expected a syntax error")
		}
	}
}

// TestPathologicalBacktracking is the packrat regression gate: well past
// upstream's 19-paren case must still fail fast. If memoization breaks,
// this does not finish.
func TestPathologicalBacktracking(t *testing.T) {
	for _, n := range []int{19, 40} {
		pe := rejectError(t, unmatchedParens(n))
		if !strings.Contains(pe.Msg, "syntax error") {
			t.Errorf("unmatchedParens(%d): unexpected message %q", n, pe.Msg)
		}
	}
}

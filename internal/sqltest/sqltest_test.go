package sqltest

import (
	"strings"
	"testing"
)

const sample = `# name: test/sql/sample.test
# group: [sample]

statement ok
CREATE TABLE a (i INTEGER);
# trailing comment glued to the record must not join the SQL

query I
SELECT i
FROM a
----
42

statement error
SELECT )
----
Parser Error

# a query whose results could be mistaken for directives
query I
SELECT 'statement ok'
----
statement ok

skipif threads=1
statement ok
SELECT 2

loop i 0 10

statement ok
SELECT ${i}

statement ok
SELECT 'literal inside loop'

endloop

require parquet

statement maybe
COPY a TO 'x.parquet'
----
IO Error
`

func TestExtract(t *testing.T) {
	stmts, err := Extract(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE TABLE a (i INTEGER);",
		"SELECT i\nFROM a",
		"SELECT )",
		"SELECT 'statement ok'",
		"SELECT 2",
		// SELECT ${i} skipped: template substitution
		"SELECT 'literal inside loop'",
		"COPY a TO 'x.parquet'",
	}
	if len(stmts) != len(want) {
		var got []string
		for _, s := range stmts {
			got = append(got, s.SQL)
		}
		t.Fatalf("extracted %d statements %q, want %d", len(stmts), got, len(want))
	}
	for i, w := range want {
		if stmts[i].SQL != w {
			t.Errorf("statement %d = %q, want %q", i, stmts[i].SQL, w)
		}
	}
	// line numbers point at the first SQL line
	if stmts[0].Line != 5 {
		t.Errorf("statement 0 line = %d, want 5", stmts[0].Line)
	}
}

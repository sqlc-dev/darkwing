package parser

import (
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/serialize"
)

// TestV2QueryErrors pins the Parser Errors the v2.0 query/expression
// transformers raise (each verified against the pinned v2.0.0 oracle).
func TestV2QueryErrors(t *testing.T) {
	const depth = `Max expression depth limit of 1000 exceeded. Use "SET max_expression_depth TO x" to increase the maximum expression depth.`
	for _, tc := range []struct{ sql, want string }{
		{"SELECT a IS NULL = b = c", "Chained comparisons are not supported, use AND to combine comparisons"},
		{"SELECT a NOT !~~ 'x'", "NOT cannot precede an operator that is already negated (!~~, !~~*, !~ or !~*)"},
		{"SELECT a ~* 'x' ESCAPE 'y'", "ESCAPE clause is not supported with case-insensitive regular expression match operators"},
		{"SELECT count(DISTINCT)", "DISTINCT requires at least one argument"},
		{"SELECT count(ALL)", "ALL requires at least one argument"},
		{"SELECT Foo(x) WITHIN GROUP (ORDER BY x)", `Unknown ordered aggregate "Foo".`},
		{"SELECT mode(DISTINCT x) WITHIN GROUP (ORDER BY x)", "DISTINCT is not allowed in combination with WITHIN GROUP"},
		{"SELECT mode(ORDER BY x) WITHIN GROUP (ORDER BY x)", "Cannot use multiple ORDER BY statements with WITHIN GROUP"},
		{"SELECT (x).ifnull(DISTINCT 1)", "DISTINCT is not supported in IFNULL expressions"},
		{"SELECT (x).ifnull(1 ORDER BY 1)", "ORDER BY is not supported in IFNULL expressions"},
		{"SELECT x.ifnull(1)", "Wrong number of arguments to IFNULL."},
		{`SELECT "unpack"(a := 1)`, "Named arguments are not supported in UNPACK operator"},
		{"SELECT sum(x) OVER (ROWS BETWEEN CURRENT ROW AND 1 PRECEDING)", "Frame starting from current row cannot have preceding rows"},
		{"SELECT sum(x) OVER (ROWS BETWEEN 1 FOLLOWING AND CURRENT ROW)", "Frame starting from following row cannot have preceding rows"},
		{"SELECT sum(x) OVER (ROWS 1 FOLLOWING)", "Frame starting from following row cannot end with current row"},
		{"SELECT sum(x) OVER w FROM t WINDOW w AS (), w AS ()", `window "w" is already defined`},
		{"SELECT (SELECT sum(x) OVER w) FROM t WINDOW w AS ()", `window "w" does not exist`},
		{"SELECT $0", "Parameter numbers must be greater than 0"},
		{"SELECT #99999999999999999999", "Positional reference node needs to be >= 1"},
		{"SELECT x::TIMESTAMP(3) WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE does not take a precision"},
		{"SELECT x::TIMESTAMP(NULL)", "TIMESTAMP precision cannot be NULL"},
		{"SELECT x::TIMESTAMP(1.5)", "TIMESTAMP precision must be an integral type"},
		{"SELECT X'0'", "Hex string literal must have an even number of hex digits"},
		{"SELECT X'0G'", "Hex string literal contains a non-hexadecimal character 'G'"},
		{"SELECT B'012'", "Bit string literal contains a non-binary character '2'"},
		{`WITH "a""b" AS (SELECT 1), "a""b" AS (SELECT 2) SELECT 1`, `Duplicate CTE name "a""b"`},
		{"SELECT * FROM t: (SELECT 1) AS u", "Table reference (SELECT 1) cannot have two aliases"},
		{"SELECT * FROM LATERAL (SELECT 1) TABLESAMPLE 10%", "TABLESAMPLE cannot be applied to a LATERAL subquery"},
		{"SELECT * FROM LATERAL range(3) TABLESAMPLE 10%", "TABLESAMPLE cannot be applied to a LATERAL table function"},
		{"PIVOT t", "PIVOT requires an ON, USING or GROUP BY clause"},
		{"PIVOT t ON a IN (x.y)", "PIVOT IN list cannot contain qualified column references"},
		{"SELECT * EXCLUDE (i) REPLACE (i+100 AS i) FROM integers", `Column ""i"" cannot occur in both EXCLUDE and REPLACE list`},
		{"SELECT * EXCLUDE (a.b.c.d.e) FROM t", "Expected at most 4 entries (catalog.schema.table.column), but found 5 entries (input: a.b.c.d.e)"},
		{"SUMMARIZE", "Expected table name with SUMMARIZE"},
		{"SHOW ''", "Expected a table name, but got an empty string"},
		{"SELECT 1 + ANY([1, 2])", `Unsupported comparison "+" for ANY/ALL subquery`},
		{"SELECT " + strings.Repeat("(", 1000) + "1" + strings.Repeat(")", 1000), depth},
		{"SELECT " + strings.Repeat("NOT ", 1000) + "true", depth},
	} {
		if got := parseErr(t, tc.sql); got != tc.want {
			t.Errorf("Parse(%.60q):\n got %q\nwant %q", tc.sql, got, tc.want)
		}
	}
	// one below the limit parses
	parseOne(t, "SELECT "+strings.Repeat("(", 999)+"1"+strings.Repeat(")", 999))
	parseOne(t, "SELECT "+strings.Repeat("NOT ", 999)+"true")
}

// TestV2Shapes spot-checks v2.0 tree shapes (oracle-verified): literals
// keep their text, IF stays a function, omitted slice bounds are
// list_value(), and nested schema paths survive in qualified_name.
func TestV2Shapes(t *testing.T) {
	sel := func(sql string) *ast.SelectNode {
		t.Helper()
		return parseOne(t, sql).(*ast.SelectStatement).Node.(*ast.SelectNode)
	}
	n := sel("SELECT -9223372036854775808, 1.50, if(a, 1, 2), a[1:], s1.s2.s3.f(1)")
	if c := n.SelectList[0].(*ast.ConstantExpression); c.Literal != (ast.Literal{Kind: ast.LiteralInteger, Text: "-9223372036854775808"}) {
		t.Errorf("negated literal = %+v", c.Literal)
	}
	if c := n.SelectList[1].(*ast.ConstantExpression); c.Literal != (ast.Literal{Kind: ast.LiteralNumeric, Text: "1.50"}) {
		t.Errorf("numeric literal = %+v", c.Literal)
	}
	if f, ok := n.SelectList[2].(*ast.FunctionExpression); !ok || f.FunctionName != "if" {
		t.Errorf("IF = %T, want FunctionExpression if", n.SelectList[2])
	}
	slice := n.SelectList[3].(*ast.OperatorExpression)
	if f, ok := slice.Operands[2].(*ast.FunctionExpression); !ok || f.FunctionName != "list_value" || len(f.Arguments) != 0 {
		t.Errorf("open slice end = %T", slice.Operands[2])
	}
	f := n.SelectList[4].(*ast.FunctionExpression)
	if f.Catalog != "s1" || f.Schema != "s3" || strings.Join(f.Qualification, ".") != "s1.s2.s3" {
		t.Errorf("nested function = %q %q %v", f.Catalog, f.Schema, f.Qualification)
	}
	out, err := serialize.SelectStatementJSON(&ast.SelectStatement{Node: n})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"literal":{"kind":"INTEGER","text":"-9223372036854775808"}`,
		`"qualified_name":{"path":["s1","s2","s3","f"]}`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("serialization lacks %s", want)
		}
	}
}

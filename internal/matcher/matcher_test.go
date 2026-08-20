package matcher

import (
	"errors"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/grammar"
	"github.com/sqlc-dev/darkwing/lexer"
	"github.com/sqlc-dev/darkwing/token"
)

// Compiling the full vendored grammar from the Program root is the
// milestone-1 engine gate: it proves every reachable rule compiles and
// that no reachable rule needs the (unsupported) character-class tokens —
// those rules are all satisfied by overrides.
func TestDefaultEngineCompiles(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if engine.topLevel == nil {
		t.Fatal("TopLevelStatement matcher missing")
	}
}

// Every rule override must name a rule the vendored grammar defines (or,
// for EndOfInput, the implicit rule the loader adds), so the transcribed
// table cannot drift silently from upstream.
func TestOverridesExistInGrammar(t *testing.T) {
	g, err := grammar.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range OverrideRuleNames() {
		if g.Rule(name) == nil {
			t.Errorf("override %q has no rule in the vendored grammar", name)
		}
	}
}

// Rules whose definitions use character-class tokens (Regex) cannot be
// compiled by the matcher factory; every one of them must be shadowed by
// a rule override on any path from Program (the overrides short-circuit
// compilation, so rules like PlainIdentifier behind the Identifier
// override are never compiled). This is the "special rules" walk from the
// plan: walk the grammar from Program, stopping at overridden rules, and
// require that no reached rule needs a character-class token.
func TestRegexRulesAreOverridden(t *testing.T) {
	g, err := grammar.Load()
	if err != nil {
		t.Fatal(err)
	}
	overridden := make(map[string]bool)
	for _, name := range OverrideRuleNames() {
		overridden[strings.ToLower(name)] = true
	}
	visited := make(map[string]bool)
	var walk func(name string)
	walk = func(name string) {
		key := strings.ToLower(name)
		if visited[key] || overridden[key] {
			return
		}
		visited[key] = true
		rule := g.Rule(name)
		if rule == nil {
			t.Errorf("reachable rule %q is missing", name)
			return
		}
		for _, tok := range rule.Tokens {
			switch tok.Type {
			case grammar.Regex:
				t.Errorf("reachable rule %q uses a character-class token but has no override", name)
			case grammar.Reference, grammar.FunctionCall:
				if _, isParam := rule.Parameters[tok.Text]; !isParam {
					walk(tok.Text)
				}
			}
		}
	}
	walk("Program")
}

func mustTokens(t *testing.T, sql string) []token.Token {
	t.Helper()
	tokens, err := lexer.Tokenize(sql)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", sql, err)
	}
	return tokens
}

// matchAll drives the statement-at-a-time loop over the whole input.
func matchAll(t *testing.T, engine *Engine, sql string) ([]ParseResult, error) {
	t.Helper()
	tokens := mustTokens(t, sql)
	var results []ParseResult
	pos := 0
	for pos < len(tokens) {
		r, newPos, err := engine.MatchTopLevel(tokens, pos)
		if err != nil {
			return nil, err
		}
		if newPos == pos {
			t.Fatalf("no progress at token %d of %q", pos, sql)
		}
		results = append(results, r)
		pos = newPos
	}
	return results, nil
}

func TestAcceptStatements(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	// a milestone-1 smoke set spanning the dialect surface; the full
	// corpus gate is milestone 2
	accepted := []string{
		"SELECT 1",
		"SELECT * FROM t",
		"SELECT a, b FROM t WHERE x = 1 GROUP BY ALL ORDER BY ALL",
		"FROM t SELECT x",
		"FROM t",
		"SELECT * EXCLUDE (a) REPLACE (b + 1 AS b) FROM t",
		"1 + 1",
		"SELECT [1, 2, 3]",
		"SELECT {'a': 1}",
		"SELECT x -> x + 1",
		"SELECT l[1:3]",
		"SELECT CAST(x AS INTEGER)",
		"SELECT TRY_CAST(x AS INTEGER)",
		"SELECT x::INTEGER",
		"SELECT count(*)",
		"SELECT $1, $name, ?",
		"SELECT 'it''s'",
		"SELECT E'a\\'b'",
		"SELECT $$dollar$$",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
		"SELECT DISTINCT ON (a) a, b FROM t",
		"SELECT a FROM t UNION BY NAME SELECT b FROM u",
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name VARCHAR)",
		"CREATE OR REPLACE TEMP TABLE t AS SELECT 1",
		"INSERT INTO t VALUES (1, 2)",
		"INSERT INTO t BY NAME SELECT 1 AS a",
		"UPDATE t SET x = 1 WHERE y = 2",
		"DELETE FROM t WHERE x = 1",
		"DROP TABLE IF EXISTS t",
		"ALTER TABLE t ADD COLUMN c INTEGER",
		"ATTACH 'file.db' AS db",
		"DETACH db",
		"USE db",
		"COPY t TO 'out.parquet' (FORMAT parquet)",
		"EXPORT DATABASE 'dir'",
		"INSTALL httpfs",
		"LOAD httpfs",
		"PRAGMA table_info('t')",
		"SET threads = 4",
		"RESET threads",
		"BEGIN TRANSACTION",
		"COMMIT",
		"CHECKPOINT",
		"VACUUM",
		"TRUNCATE t",
		"COMMENT ON TABLE t IS 'hi'",
		"PREPARE q AS SELECT $1",
		"EXECUTE q(1)",
		"DEALLOCATE q",
		"EXPLAIN SELECT 1",
		"DESCRIBE t",
		"SHOW TABLES",
		"SUMMARIZE t",
		"PIVOT tbl ON col USING sum(v)",
		"UNPIVOT tbl ON a, b",
		"SELECT 1; SELECT 2;",
		"SELECT interval '1 hour'",
		"SELECT x AT TIME ZONE 'UTC'",
		"SELECT a FROM t QUALIFY row_number() OVER (PARTITION BY b) = 1",
		"SELECT * FROM t USING SAMPLE 10%",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = 1",
		"SELECT struct.field FROM t",
		`SELECT "quoted col" FROM "quoted table"`,
		"CREATE SCHEMA s",
		"CREATE SEQUENCE seq",
		"CREATE TYPE mood AS ENUM ('happy', 'sad')",
		"CREATE MACRO add1(x) AS x + 1",
		"CREATE VIEW v AS SELECT 1",
		"CREATE INDEX idx ON t (a)",
	}
	for _, sql := range accepted {
		if _, err := matchAll(t, engine, sql); err != nil {
			t.Errorf("accept %q: %v", sql, err)
		}
	}
}

func TestRejectStatements(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rejected := []struct {
		sql  string
		near string
	}{
		{"SELECT (", "("},
		{"SELECT 1 +", "+"},
		{"SELECT FROM FROM", "FROM"},
		{"CREATE 1", "1"},
		{"SELECT * FROM WHERE", "WHERE"},
		{"INSERT INTO", "INTO"},
	}
	for _, tc := range rejected {
		_, err := matchAll(t, engine, tc.sql)
		var syntaxErr *SyntaxError
		if !errors.As(err, &syntaxErr) {
			t.Errorf("reject %q: err = %v, want *SyntaxError", tc.sql, err)
			continue
		}
		if syntaxErr.Token.Text != tc.near {
			t.Errorf("reject %q: at or near %q, want %q", tc.sql, syntaxErr.Token.Text, tc.near)
		}
		if want := "syntax error at or near " + `"` + tc.near + `"`; syntaxErr.Error() != want {
			t.Errorf("reject %q: message %q, want %q", tc.sql, syntaxErr.Error(), want)
		}
	}
}

// Reserved keywords are rejected as bare identifiers but allowed quoted;
// category keywords are accepted in their positions.
func TestKeywordCategoryMatching(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := matchAll(t, engine, `SELECT x FROM "select"`); err != nil {
		t.Errorf("quoted reserved keyword as table name: %v", err)
	}
	// GENERATED is a column-name keyword: valid as a bare column reference
	if _, err := matchAll(t, engine, "SELECT generated FROM t"); err != nil {
		t.Errorf("column-name keyword as column: %v", err)
	}
	// unreserved keywords are valid everywhere identifiers are
	if _, err := matchAll(t, engine, "SELECT abort FROM abort"); err != nil {
		t.Errorf("unreserved keyword as identifier: %v", err)
	}
}

// Negative lookahead (`!`) is parsed but ignored by the matcher,
// replicating the pinned DuckDB build (acknowledged upstream TODO in
// matcher_factory.cpp). "Ignored" means only the `!` operator itself is
// dropped: its operand stays in the sequence as an ordinary CONSUMING
// element. Textbook PEG would make `!'A' 'A'` unsatisfiable; the pinned
// behavior matches two A tokens. Flip this test in lockstep with
// upstream, never independently. (The vendored grammar's only bare `!` is
// in PlainIdentifier, which sits behind the Identifier override and is
// never compiled, so nothing reachable depends on the choice today.)
func TestNegativeLookaheadIgnored(t *testing.T) {
	g, err := grammar.Parse("Root <- !'A' 'A' EndOfInput\n")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(g, "Root")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.Match(mustTokens(t, "A A"), 0); err != nil {
		t.Errorf("pinned behavior: !'A' consumes a token; A A should match: %v", err)
	}
	if _, _, err := engine.Match(mustTokens(t, "A"), 0); err == nil {
		t.Error("pinned behavior: a single A should not satisfy !'A' 'A'")
	}
}

// The `/` operator binds only the immediately preceding element: upstream
// compiles `A B / C` as A (B / C), which is why the vendored grammars
// parenthesize multi-token alternatives. Pin that quirk.
func TestChoiceBindsLastElement(t *testing.T) {
	g, err := grammar.Parse("Root <- 'A' 'B' / 'C'\n")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(g, "Root")
	if err != nil {
		t.Fatal(err)
	}
	// "A C" matches: A then (B / C)
	if _, _, err := engine.Match(mustTokens(t, "A C"), 0); err != nil {
		t.Errorf("A C should match A (B / C): %v", err)
	}
	// bare "C" does not: the leading A is required
	if _, _, err := engine.Match(mustTokens(t, "C"), 0); err == nil {
		t.Error("C alone should not match A (B / C)")
	}
}

func TestParameterizedRuleExpansion(t *testing.T) {
	g, err := grammar.Parse("Root <- Parens(List(Item)) EndOfInput\nItem <- 'A' / 'B'\nList(D) <- D (',' D)* ','?\nParens(D) <- '(' D ')'\n")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(g, "Root")
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"(A)", "(A, B)", "(A, B,)", "(A,B,A)"} {
		if _, _, err := engine.Match(mustTokens(t, sql), 0); err != nil {
			t.Errorf("match %q: %v", sql, err)
		}
	}
	for _, sql := range []string{"()", "(A", "A)", "(A B)"} {
		if _, _, err := engine.Match(mustTokens(t, sql), 0); err == nil {
			t.Errorf("match %q should fail", sql)
		}
	}
}

// The parse-result shape is what transformers index into; pin the result
// kinds for a tiny grammar.
func TestParseResultShape(t *testing.T) {
	g, err := grammar.Parse("Root <- 'GO' Name Number? EndOfInput\nName <- Identifier\nNumber <- NumberLiteral\nIdentifier <- 'x'\nNumberLiteral <- 'y'\n")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(g, "Root")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := engine.Match(mustTokens(t, "go abc 42"), 0)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := r.(*ListResult)
	if !ok {
		t.Fatalf("root = %T, want *ListResult", r)
	}
	if root.Name() != "Root" {
		t.Errorf("root name = %q", root.Name())
	}
	if len(root.Children) != 4 {
		t.Fatalf("root children = %d, want 4", len(root.Children))
	}
	kw, ok := root.Child(0).(*KeywordResult)
	if !ok || kw.Keyword != "go" {
		t.Errorf("child 0 = %#v, want keyword \"go\" (original spelling)", root.Child(0))
	}
	// Name is itself a rule, so its match is a one-child ListResult
	// wrapping the identifier override's result
	nameList, ok := root.Child(1).(*ListResult)
	if !ok || nameList.Name() != "Name" || len(nameList.Children) != 1 {
		t.Fatalf("child 1 = %#v", root.Child(1))
	}
	name, ok := nameList.Child(0).(*IdentifierResult)
	if !ok || name.Identifier != "abc" {
		t.Errorf("Name child = %#v", nameList.Child(0))
	}
	opt, ok := root.Child(2).(*OptionalResult)
	if !ok || !opt.HasResult() {
		t.Fatalf("child 2 = %#v", root.Child(2))
	}
	numList, ok := opt.Result.(*ListResult)
	if !ok || numList.Name() != "Number" || len(numList.Children) != 1 {
		t.Fatalf("optional result = %#v", opt.Result)
	}
	num, ok := numList.Child(0).(*NumberResult)
	if !ok || num.Number != "42" {
		t.Errorf("Number child = %#v", numList.Child(0))
	}
	if _, ok := root.Child(3).(*EndOfInputResult); !ok {
		t.Errorf("child 3 = %#v", root.Child(3))
	}
	// offsets: the identifier starts at byte 3
	if off, length, ok := name.Loc(); !ok || off != 3 || length != 3 {
		t.Errorf("identifier loc = (%d, %d, %v)", off, length, ok)
	}
}

// The identifier overrides unwrap quoting: "a""b" becomes a"b.
func TestIdentifierUnwrapping(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := engine.MatchTopLevel(mustTokens(t, `SELECT 1 AS "a""b"`), 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	var walk func(ParseResult)
	walk = func(n ParseResult) {
		switch n := n.(type) {
		case *IdentifierResult:
			if n.Identifier == `a"b` {
				found = true
			}
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
	if !found {
		t.Error(`quoted identifier "a""b" was not unwrapped to a"b`)
	}
}

// Upstream throws (rather than failing the alternative) when a number
// token carries two exponent markers.
func TestDoubleScientificNotationError(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	_, err = matchAll(t, engine, "SELECT 1e5e2")
	var matchErr *MatchError
	if !errors.As(err, &matchErr) {
		t.Fatalf("err = %v, want *MatchError", err)
	}
	if matchErr.Msg != "Already found scientific notation" {
		t.Errorf("msg = %q", matchErr.Msg)
	}
}

// The engine is immutable and must be race-clean under concurrent use
// (each Match call owns its packrat cache).
func TestConcurrentMatching(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		"SELECT a, b FROM t WHERE x = 1",
		"INSERT INTO t VALUES (1)",
		"SELECT ((((", // syntax error path
		"CREATE TABLE t (a INTEGER)",
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				sql := statements[(i+j)%len(statements)]
				tokens, err := lexer.Tokenize(sql)
				if err != nil {
					t.Error(err)
					return
				}
				engine.MatchTopLevel(tokens, 0)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// Packrat memoization keeps pathological backtracking linear: upstream's
// motivating case is a SELECT with 19 unmatched '(' going from 10.6s to
// ~1ms. Without memoization this test would not terminate in any
// reasonable time.
func TestPathologicalBacktracking(t *testing.T) {
	engine, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	sql := "SELECT " + strings.Repeat("(", 19)
	_, err = matchAll(t, engine, sql)
	var syntaxErr *SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("err = %v, want *SyntaxError", err)
	}
}

func BenchmarkPathologicalParens(b *testing.B) {
	engine, err := Default()
	if err != nil {
		b.Fatal(err)
	}
	tokens, err := lexer.Tokenize("SELECT " + strings.Repeat("(", 19))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		engine.MatchTopLevel(tokens, 0)
	}
}

func BenchmarkSimpleSelect(b *testing.B) {
	engine, err := Default()
	if err != nil {
		b.Fatal(err)
	}
	tokens, err := lexer.Tokenize("SELECT a, b, count(*) FROM t WHERE x = 1 AND y > 2 GROUP BY a, b ORDER BY a LIMIT 10")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := engine.MatchTopLevel(tokens, 0); err != nil {
			b.Fatal(err)
		}
	}
}

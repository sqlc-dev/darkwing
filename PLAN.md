# darkwing — a pure Go parser for the DuckDB SQL dialect

*(Darkwing Duck, the terror that flaps in the night, for DuckDB)*

darkwing is a hand-written, zero-dependency Go port of **DuckDB v2.0's new
PEG-based SQL parser**, in the same family as:

- [sqlc-dev/oliphant](https://github.com/sqlc-dev/oliphant) — PostgreSQL (libpg_query)
- [sqlc-dev/marino](https://github.com/sqlc-dev/marino) — MySQL (TiDB fork, goyacc)
- [sqlc-dev/meyer](https://github.com/sqlc-dev/meyer) — SQLite (Lemon)
- [sqlc-dev/teesql](https://github.com/sqlc-dev/teesql) — T-SQL (SQL Server)
- [sqlc-dev/zetajones](https://github.com/sqlc-dev/zetajones) — GoogleSQL (BigQuery)
- [sqlc-dev/doubleclick](https://github.com/sqlc-dev/doubleclick) — ClickHouse

Its immediate purpose is to power a first-class DuckDB engine in
[sqlc](https://github.com/sqlc-dev/sqlc). Unlike the siblings, there is no
incumbent parser to replace — DuckDB is a **new** dialect for sqlc, so the
public API and AST can be designed for sqlc's needs from day one.

## Reference implementation and pin

DuckDB v2.0 replaces its PostgreSQL-derived YACC parser with an in-house PEG
parser (merged in [duckdb/duckdb#22194](https://github.com/duckdb/duckdb/pull/22194);
announced in [the v2.0 preview](https://duckdb.org/2026/08/17/duckdb-20-highlights)
and described in depth in
["Your Database Deserves a Better Parser"](https://duckdb.org/2026/08/20/duckdb-20-peg-parser),
building on the 2024
[runtime-extensible parsers post](https://duckdb.org/2024/11/22/runtime-extensible-parsers)
and a CIDR 2026 paper). The old libpg_query parser is deleted from the tree.

Everything lives under `src/parser/peg/` in the DuckDB repo:

| Component | Upstream | Size |
|---|---|---|
| Grammar | `grammar/statements/*.gram` (39 files) | ~1,400 lines |
| Keyword lists | `grammar/keywords/*.list` (5 files) | 528 keywords |
| Tokenizer | `tokenizer/base_tokenizer.cpp` + `parser_tokenizer.cpp` | ~600 lines |
| Grammar loader | `peg_parser.cpp`, `parsed_grammar.cpp` | ~330 lines |
| Matcher | `matcher_factory.cpp`, `matcher.cpp`, `matcher/*.hpp` | ~550 lines + headers |
| Transformer | `transformer/*.cpp` (46 files) | ~48,000 lines |

**Initial pin:** DuckDB `main` at a recorded commit hash (v2.0 is not tagged
yet as of 2026-08-20; the parser extension API is explicitly preview-status).
The pin advances to the `v2.0.0` tag the week it lands, via the § Regeneration
workflow — as in oliphant, "advance the pin" is a first-class workflow, not an
event. Until 2.0 ships, expect grammar churn and budget one pin advance per
upstream nightly we choose to chase (weekly is plenty).

## The central design decision: port the engine, not transcribe the grammar

The siblings are hand-written recursive descent because their upstream
grammars are toolchain artifacts (Lemon, YACC, ANTLR): the only way to be
zero-dependency was to re-express the grammar as Go functions. DuckDB is
different in kind: **upstream's own production parser is an interpreter over
a machine-readable grammar text.** DuckDB parses the `.gram` files at startup
into rule objects, compiles them to a matcher tree, and matches tokens
against it. The grammar text is not an input to a build step — it *is* the
parser's data.

darkwing therefore ports the architecture faithfully instead of flattening it
into recursive descent:

1. **Vendor the grammar verbatim.** The `.gram` and `.list` files are copied
   unmodified from the pinned DuckDB commit and embedded with `go:embed`.
2. **Port the engine.** Tokenizer, grammar loader, matcher (with packrat
   memoization) — ~1,500 lines of C++ that become a comparable amount of Go.
3. **Hand-write the transformer.** Generic parse tree → typed Go AST. This is
   the bulk of the work, exactly as it is the bulk of upstream (48k lines).

Why this beats recursive descent here:

- **Accept/reject conformance is structural.** Whether darkwing accepts a
  statement is decided entirely by the engine + the vendored grammar — the
  transformer only shapes the tree. The full corpus accept/reject gate can go
  green in milestone 2, before a single transformer exists. A recursive
  descent port cannot separate those concerns.
- **Pin advances are diffs, not rewrites.** When DuckDB 2.1 adds syntax, the
  grammar diff is re-vendored mechanically; only genuinely new rules need new
  transformer functions. Recursive descent would need every grammar change
  re-derived by hand. This matters more for DuckDB than any sibling dialect:
  the whole point of the PEG parser is that DuckDB's grammar now evolves
  quickly (extensions add syntax at runtime).
- **PEG semantics come for free.** Ordered choice ("first match wins",
  specific-before-general ordering like `YEAR TO MONTH` before `YEAR`) is
  exactly what the engine does; replicating those commitment points manually
  is where a hand-port would silently diverge.

What does **not** change from the family playbook: zero dependencies (the
engine is hand-written Go; no parser generator, no codegen step in the build,
`go.mod` is the module line and the Go version), and the grammar files carry
provenance the same way meyer vendors `parse.y` — except here they are
load-bearing, so they are covered by the conformance suite rather than by
documentation tests.

One upstream quirk is a hard conformance rule: **negative lookahead (`!`) is
parsed but ignored by DuckDB's matcher** (an acknowledged upstream TODO).
darkwing must replicate the *effective* behavior of the pinned build, not
textbook PEG semantics — so darkwing's matcher ignores `!` too, with a test
pinning that choice and a note to flip it in lockstep when upstream does.

## The four components

### 1. Tokenizer (`lexer/`)

A port of `base_tokenizer.cpp`. DuckDB's PEG matcher is **token-level, not
character-level**: the tokenizer runs first, strips comments, and emits
tokens with byte offsets; the matcher never sees raw text. Lexical territory
(PostgreSQL-flavored, with DuckDB additions):

- `'...'` strings (doubled-quote escape), `E'...'` escape strings,
  `"..."` quoted identifiers, **dollar-quoted strings** `$tag$...$tag$`
  (empty and named tags, the `$[0-9]` disambiguation rule: `$1` is a
  parameter, never a dollar-quote opener).
- **Parameter markers, first-class for sqlc**: `?` (positional),
  `$1` (numbered), `$name` ("collabel" parameters — the tokenizer decides
  `$foo` is a parameter only when no closing `$` follows a valid tag).
- Numbers (ints, decimals, scientific, hex, underscore separators),
  operators (PostgreSQL-style multi-char operator tokens, `::` casts,
  `->`/`->>` JSON operators, `**`, `//`), `$` as a valid non-initial
  identifier character.
- `--` line and `/* */` block comments (nesting per upstream), filtered
  before matching but never attributed to a token's span.
- Keywords are case-insensitive; the token records the keyword's **category**
  (see § Keywords) plus its raw spelling and byte span.

The tokenizer also underpins upstream's autocomplete/highlighting; darkwing
ports only the parsing tokenizer (`parser_tokenizer.cpp` behavior).

### 2. Grammar loader (`internal/grammar/`)

A port of `peg_parser.cpp` + `parsed_grammar.cpp`: parse the embedded `.gram`
text into rule objects at package init (`sync.Once`, then immutable and
goroutine-safe). Grammar syntax to support, per upstream's
`src/parser/peg/README.md`:

- `Rule <- definition`, sequences, ordered choice `/`, `?` `*` `+`,
  grouping parens, `#` comments, rules starting with `%` (`%whitespace`).
- **Parameterized rules (macros)**: `List(D) <- D (',' D)* ','?` and
  `Parens(D) <- '(' D ')'` from `common.gram`, with nesting
  (`Parens(List(Expression))`).
- Single-quoted literals (case-insensitive keyword/operator matches),
  `<[a-z_]i[a-z0-9_]i*>` character-class captures, `!` negative lookahead
  (parsed, ignored — see above).
- Duplicate rule names are a load-time error; loading the vendored grammar
  successfully is itself a test.

### 3. Matcher (`internal/matcher/`)

A port of `matcher_factory.cpp` + `matcher.cpp`: compile rules into a matcher
tree (list/choice/optional/repeat/keyword/literal matchers), then match the
token stream producing a generic parse tree (`ParseResult`: list, choice,
optional, repeat, identifier, string, number, keyword — mirroring upstream's
types since the transformer indexes into them by grammar position).

- **Packrat memoization** to kill exponential backtracking (upstream
  memoizes designated hot rules — `LogicalOrExpression`,
  `ComparisonExpression`, `AdditiveExpression`, etc.; darkwing ports the same
  list and keeps the mechanism general). Upstream's motivating case — 19
  unmatched `(` going from 10.6 s to 1 ms — becomes a benchmark.
- **Rule overrides**: special tokens handled natively rather than by grammar
  expansion — `Identifier` and its context/category variants (`ColId`,
  `TableName`, `ReservedColumnName`, `FunctionName`, …), `StringLiteral`,
  `NumberLiteral`, `OperatorLiteral`, `EndOfInput`. The override list is
  transcribed from `matcher_factory.cpp` and checked by a test that walks the
  vendored grammar for special rules with no definition.
- **Error reporting**: furthest-failure position + expected-token set, the
  basis for `Parser Error: syntax error at or near "X"` with line/column —
  matching the pinned build's messages and positions is a conformance goal
  (§ Oracle), and the blog markets the improved errors, so fidelity here is
  user-visible.

### 4. Transformer (`parser/transform_*.go`)

The real work: one transformer function per grammar rule that produces an AST
node, registered in a `map[string]transformFunc` keyed by rule name
(upstream's `REGISTER_TRANSFORM` pattern; upstream generates trampolines from
`grammar_types.yml`, darkwing hand-writes the registration table — a
`cmd/`-level dev tool cross-checks that every rule reachable from `Statement`
either has a transformer or is consumed structurally by its parent). Each
function carries an attribution comment naming its upstream
`transform_*.cpp` counterpart, zetajones-style.

## Keywords and identifier classes

DuckDB classifies every keyword into **exactly one of five categories**
(`reserved` 75, `unreserved` 339, `column_name` 54, `func_name` 29,
`type_name` 31 — 528 total), and the grammar's identifier rules consume them
through category-aware special tokens (`ColId` accepts unreserved +
column-name keywords; `FunctionName` accepts func-name keywords; plain
`Identifier` rejects reserved keywords; `ReservedIdentifier` variants accept
even reserved ones in qualified positions, e.g. `catalog.schema."select"`).
This is the PEG analog of PostgreSQL's `col_name_keyword`/`type_func_name_keyword`
split and is most of the accept/reject battle. The `.list` files are vendored
and embedded; the exactly-one-category invariant is a load-time test. The
keyword table is also what sqlc's `reserved.go` generation will consume.

## Dialect surface (what makes this not-Postgres)

The grammar is compact but the dialect is broad — 37 top-level statement
types including DuckDB natives with no sibling precedent:

- **FROM-first selects** (`FROM t SELECT x`, bare `FROM t`), `SELECT * EXCLUDE
  (...) REPLACE (...) RENAME (...)`, star with lambda filters, `COLUMNS(...)`,
  `QUALIFY`, `SAMPLE`/`USING SAMPLE`, `GROUP BY ALL`/`ORDER BY ALL`,
  `UNION BY NAME`, `DISTINCT ON`, WITH ... USING KEY, prefix aliases
  (`x: expr`), `TABLE t`, chained comparison bans, trailing commas.
- **Bare expression statements** (`Statement`'s final alternative —
  `1 + 1` is a valid statement), `SUMMARIZE`, `DESCRIBE`/`SHOW`.
- **PIVOT / UNPIVOT / PIVOT_WIDER / PIVOT_LONGER** (both the statement forms
  and the table-ref forms), `MERGE INTO`.
- `ATTACH`/`DETACH`/`USE`/`CONNECT`/`DISCONNECT`, `COPY ... TO/FROM`
  (option soup), `EXPORT`/`IMPORT DATABASE`, `INSTALL`/`LOAD`/
  `UPDATE EXTENSIONS`, `CREATE MACRO`/`SECRET`/`SEQUENCE`/`TYPE`,
  `CHECKPOINT`, `TRUNCATE`, `COMMENT ON`, `VACUUM`, `SET`/`RESET`
  (+ scoped variants), `PRAGMA` (call and assignment forms), prepared
  statements (`PREPARE`/`EXECUTE`/`DEALLOCATE`).
- Expression layer: lambdas (`x -> x + 1`), `TRY_CAST`, `::` casts,
  list/struct/map literals, list comprehensions, slicing (`l[1:3]`),
  `AT TIME ZONE`, `WITHIN GROUP`, `FILTER`, `IGNORE/RESPECT NULLS`,
  named function arguments (`:=` and `=>`), interval qualifiers
  (`YEAR TO MONTH`), `POSITION(x IN y)`, star expressions inside function
  calls (`count(*)` is grammar, not special case).

The precedence ladder is expressed *in* the grammar
(`Expression <- LambdaArrowExpression`, descending through OR → AND → NOT →
IS → comparison → BETWEEN/IN/LIKE → collate/at-time-zone → additive →
multiplicative → exponentiation → prefix/postfix → indexing/casting), so
darkwing inherits it from the vendored text rather than implementing it —
another argument for the engine port.

## Conformance oracle and corpus

| repo | golden output | generated by |
|---|---|---|
| zetajones | `ASTNode::DebugString` trees | vendored upstream testdata |
| teesql | ScriptDom JSON | bundled C# tool |
| doubleclick | `EXPLAIN AST` text | pinned ClickHouse binary |
| meyer | accept/reject + message + offset | pinned SQLite build |
| oliphant | pg_query protobuf JSON | pinned libpg_query build |
| **darkwing** | **accept/reject + message/position, plus `json_serialize_sql` JSON for selects** | **pinned DuckDB binary** |

**Corpus.** DuckDB's own test suite: `test/sql/**/*.test` (~4,700 files,
sqllogictest dialect — `statement ok` / `statement error` (+ expected-message
fragment after `----`) / `query` blocks). `cmd/regenerate` extracts every SQL
statement with a small sqllogictest reader (skipping `loop`/`foreach`
template substitutions, keeping everything literal), plus the
`test/fuzzer/` regression corpus as the robustness set. Consolidated
meyer-style storage — one corpus file per upstream file, cases separated by
`==`, SQL and expectation separated by `--`, one `metadata.json` todo/skip
sidecar per file; single-digit MB, never doubleclick's 127k-file explosion.
Expected outputs are **never edited by hand**.

**Accept/reject oracle.** Every extracted statement runs through the pinned
official DuckDB CLI against `:memory:`. Classification: error message
starting with `Parser Error` → must-reject (message + position recorded);
success or any post-parse error (`Binder Error`, `Catalog Error`, …) →
must-accept. Same rule as meyer: statements that die *after* parsing are
must-accept for darkwing. Extension-gated syntax is whatever the pinned
binary's parser accepts (the grammar is monolithic at parse time; autoloading
affects binding, not parsing).

**Tree-shape oracle.** DuckDB *can* dump parse trees, unlike SQLite:
`json_serialize_sql('SELECT ...')` emits the internal AST as JSON (SELECT
statements only). Because darkwing's AST deliberately mirrors DuckDB's node
shapes (§ AST), `internal/serialize` renders darkwing's tree in
`json_serialize_sql`-compatible JSON and diffs it against vendored oracle
output for the SELECT subset of the corpus — doubleclick's EXPLAIN-AST
technique upgraded to structured JSON. Volatile fields (`query_location`
differences, version stamps) are normalized by the comparator, with each
normalization documented. For non-SELECT statements, meyer's substitutes
apply: self-maintained AST snapshots (`internal/dump`, human-reviewed,
regenerated by tooling) and the round-trip property
(`parse(render(parse(x))) == parse(x)` via a test-only `ast.String()`
renderer that promises re-parseability, nothing more).

**Differential loop.** `cmd/difftest` mutates corpus seeds and compares
darkwing's accept/reject/message against the pinned binary — the strongest
check on the matcher's ordered-choice fidelity, and cheap to run for hours in
CI's cron lane.

## AST shape (summary)

Package `ast`, DuckDB-native naming — node names follow DuckDB's parsed
statement classes so the transformer and serializer read side-by-side with
upstream, and sqlc's future `convert.go` owns the mapping to sqlc's `sql/ast`:

- Interfaces: `Node` (`Pos`, `End`, `Children`), `Stmt`, `Expr`, `TableRef`
  with unexported marker methods; every node embeds `Span{Start, End int}`
  (byte offsets), JSON tags on everything.
- Statements (~37): `SelectStatement` (wrapping a `QueryNode` tree:
  `SelectNode`, `SetOperationNode`, `RecursiveCTENode`, `ValuesNode` —
  DuckDB's shape), `InsertStatement` (+ `OnConflictInfo`, `RETURNING`,
  BY NAME/POSITION), `UpdateStatement`, `DeleteStatement` (+ USING),
  `MergeIntoStatement`, `CreateStatement` variants (table / view / index /
  schema / sequence / type / macro / secret), `AlterStatement`,
  `DropStatement`, `CopyStatement`, `AttachStatement`/`DetachStatement`,
  `UseStatement`, `PivotStatement`/`UnpivotStatement`, `CallStatement`,
  `PragmaStatement`, `SetStatement`/`ResetStatement`, `ExplainStatement`,
  `PrepareStatement`/`ExecuteStatement`/`DeallocateStatement`,
  `TransactionStatement`, `ExportStatement`/`ImportStatement`,
  `LoadStatement`/`InstallStatement`, `VacuumStatement`,
  `CheckpointStatement`, `TruncateStatement`, `CommentOnStatement`,
  `AnalyzeStatement`, `DescribeStatement`/`ShowStatement`,
  `ExpressionStatement`.
- Expressions (~22, DuckDB's `ParsedExpression` classes):
  `ColumnRefExpression`, `ConstantExpression`, `ParameterExpression`
  (**kind + number + name preserved — the single most important node for
  sqlc's parameter inference**), `FunctionExpression` (with DISTINCT,
  ORDER BY, FILTER, WITHIN GROUP, EXPORT_STATE, IGNORE/RESPECT NULLS),
  `OperatorExpression`, `ComparisonExpression`, `ConjunctionExpression`,
  `CastExpression`, `CaseExpression`, `SubqueryExpression`,
  `WindowExpression`, `StarExpression` (EXCLUDE/REPLACE/RENAME/COLUMNS),
  `LambdaExpression`, `BetweenExpression`, `CollateExpression`,
  `PositionalReferenceExpression` (`#1`), `DefaultExpression`, plus literal
  structs for list/struct/map/interval.
- Table refs: `BaseTableRef` (+ AT clause for time travel), `JoinRef`
  (incl. ASOF/POSITIONAL/ANTI/SEMI/LATERAL), `SubqueryRef`,
  `TableFunctionRef`, `PivotRef`, `ValuesRef`, `ShowRef`, `EmptyTableRef`.
- Supporting: `TypeName` (DuckDB's rich types: `STRUCT(...)`, `MAP(...)`,
  `UNION(...)`, `LIST`/`[]` suffixes, `DECIMAL(p,s)`, enums), `ColumnDef`,
  constraints, `CommonTableExpr` (+ USING KEY, MATERIALIZED),
  `OrderModifier`/`LimitModifier`, `SampleOptions`, `WindowDef`/frames,
  `CopyOptions`, `GenericOption` (the COPY/ATTACH/SECRET option soup).

Spans are load-bearing exactly as in meyer: sqlc slices original source by
statement span to find `-- name:` comments, so `Parse` returns statements
whose spans tile the input (empty statements skipped, never breaking span
accounting), and `parser.Error` carries byte offset + rendered
`Parser Error: syntax error at or near "X"` with line/column.

## Public API

Mirrors the family so sqlc integration is uniform:

```go
package parser // github.com/sqlc-dev/darkwing/parser

func Parse(ctx context.Context, r io.Reader) ([]ast.Stmt, error)
```

plus `ParseStatement` / `ParseExpr` helpers. The engine
(`internal/grammar`, `internal/matcher`) stays internal in v1; if DuckDB's
extension-grammar story matures, exposing rule injection is a v2 decision,
not a v1 API commitment.

## License

DuckDB is MIT (grammar, source, and tests alike) — clean, like meyer's
public-domain situation but with attribution. darkwing is **MIT**, copyright
the sqlc authors, with `LICENSE.DUCKDB` reproducing upstream's MIT notice.
Vendored grammar/keyword files and the extracted corpus record provenance
(upstream commit hash + SHA-256) in `internal/grammar/README.md` and
`parser/testdata/README.md`.

## Repository layout

```
darkwing/
├── go.mod                    # github.com/sqlc-dev/darkwing — no deps
├── LICENSE                   # MIT
├── LICENSE.DUCKDB            # upstream MIT notice
├── PLAN.md                   # this file
├── CLAUDE.md                 # dev-loop guide (next-test → implement → -check-parse)
├── token/                    # Kind, Token{Kind, Text, Span}, keyword categories
├── lexer/                    # port of base_tokenizer.cpp
├── ast/                      # nodes, Span, Children, test renderer (String)
├── parser/
│   ├── parser.go             # Parse/ParseStatement/ParseExpr, statement splitting
│   ├── transform.go          # transformer registry + dispatch
│   ├── transform_select.go   # one file per upstream transform_*.cpp cluster
│   ├── transform_expr.go
│   ├── transform_ddl.go … transform_misc.go
│   ├── parser_test.go        # corpus harness (+ -check-parse)
│   ├── serialize_test.go     # json_serialize_sql goldens (SELECT subset)
│   ├── errors_test.go        # message/position fidelity
│   ├── roundtrip_test.go, span_test.go, snapshot_test.go, fuzz_test.go
│   ├── grammar_test.go       # every reachable rule transformed or consumed
│   ├── bench_test.go         # incl. the 19-parens packrat benchmark
│   └── testdata/             # README (provenance), corpus, metadata sidecars
├── internal/
│   ├── grammar/              # embedded .gram/.list files + loader (peg_parser.cpp)
│   ├── matcher/              # matcher tree, packrat, ParseResult (matcher*.cpp)
│   ├── serialize/            # json_serialize_sql-compatible JSON renderer
│   ├── dump/                 # AST snapshot renderer
│   ├── sqltest/              # sqllogictest .test reader
│   ├── testfile/             # corpus file format reader/writer
│   └── duckdbsrc/            # pinned release: download, checksum, oracle runner
└── cmd/
    ├── next-test/            # pick next todo case
    ├── debug-parse/          # parse argv SQL: tokens, raw ParseResult, or AST
    ├── difftest/             # mutation differential testing vs pinned binary
    └── regenerate/           # re-vendor grammar + extract corpus + run oracle
```

## Milestones

1. **Engine** — module, license, CI (`go build` + `go test -race`), token
   package with the five keyword categories, tokenizer port with unit tests
   transcribed from upstream tokenizer behavior, grammar loader over the
   vendored `.gram`/`.list` files (loading cleanly is the gate), matcher with
   packrat + rule overrides, `cmd/debug-parse` dumping raw `ParseResult`.
2. **Corpus + accept/reject green** — `cmd/regenerate` (sqllogictest
   extraction + pinned-binary oracle), corpus harness with todo metadata.
   Because accept/reject needs no transformers, this milestone ends with the
   headline gate: *darkwing accepts iff pinned DuckDB parses*, corpus-wide.
   Grammar-engine bugs surface here, while the surface area is still small.
3. **AST + transformer core** — `ast` package, expressions and the full
   SELECT tree (query nodes, table refs, windows, CTEs, PIVOT refs);
   `internal/serialize` + `json_serialize_sql` goldens go green over the
   SELECT subset.
4. **DML + DDL transformers** — INSERT/UPDATE/DELETE/MERGE with RETURNING
   and parameters end-to-end; CREATE/ALTER/DROP families, types, constraints.
5. **DuckDB-native statements** — COPY/ATTACH/SET/PRAGMA/macros/secrets/
   EXPORT/extension statements; snapshot coverage for everything
   `json_serialize_sql` can't see.
6. **Hardening** — error message/position fidelity over the negative corpus,
   `cmd/difftest` fuzzing, benchmarks (parse throughput + pathological
   backtracking), race-clean parallel corpus run.
7. **Pin advance to v2.0.0 final** — re-vendor grammar, re-derive corpus
   expectations, review diffs; this rehearses the § Regeneration workflow
   that every later DuckDB release will use.
8. **sqlc integration** (in the sqlc repo, separate effort) — new
   `internal/engine/duckdb`: catalog defaults, `convert.go` (darkwing AST →
   `sql/ast`), `reserved.go` from darkwing's keyword table, parameter
   inference for `?`/`$1`/`$name`, endtoend fixtures. New engine, so scope
   is minimal-viable (queries + DDL sqlc understands), gated behind
   `engine: "duckdb"`.

## Non-goals

- Not a formatter/pretty-printer (the test renderer promises only
  re-parseability).
- No semantic analysis: no binding, no catalog awareness, no type checking.
  Errors the pinned binary raises post-parse are out of scope by definition.
- No error recovery / multi-error reporting in v1 (matches upstream and all
  siblings).
- No runtime grammar extension API in v1 — the engine could support it, but
  upstream's extension API is preview-status; revisit after it stabilizes.
- No port of autocomplete/highlighting (upstream builds them on the same
  tokenizer; out of scope here).

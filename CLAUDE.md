# darkwing development guide

darkwing is a pure Go port of DuckDB v2.0's PEG parser. Read `PLAN.md` first —
it is the authoritative design document. The engine is a faithful port of the
C++ under `src/parser/peg/` in the DuckDB repo, pinned at the commit recorded
in `internal/grammar/README.md`.

## Layout

- `token/` — token kinds, `Token{Kind, Text, Span}`, the five keyword
  categories (built from the vendored `.list` files).
- `lexer/` — port of `base_tokenizer.cpp` + `parser_tokenizer.cpp`.
- `internal/grammar/` — vendored `.gram`/`.list` files (never edit them by
  hand; they are re-vendored by the regeneration workflow) and the grammar
  loader, a port of `peg_parser.cpp`/`parsed_grammar.cpp`.
- `internal/matcher/` — matcher tree, packrat memoization, rule overrides and
  `ParseResult`, a port of `matcher_factory.cpp`/`matcher.cpp`/
  `parser_packrat.cpp` and `matcher/*.hpp`.
- `cmd/debug-parse/` — dump tokens or the raw `ParseResult` tree for SQL
  passed on the command line.
- `internal/sqltest/` — sqllogictest `.test` reader (statement extraction
  only; template substitutions are skipped, never expanded).
- `internal/testfile/` — corpus storage format (cases separated by `==`,
  SQL/expectation separated by `--`) plus `*.metadata.json` todo/skip
  sidecars keyed by case content hash.
- `internal/duckdbsrc/` — the pinned DuckDB CLI (oracle): locate, verify
  version against the pin, run statements against `:memory:` and classify
  Parser Error vs post-parse.
- `cmd/regenerate/` — rebuild `parser/testdata/` from a DuckDB source tree
  + the pinned binary. The only way corpus expectations change.
- `cmd/next-test/` — print the next todo case from the corpus metadata.
- `parser/` — corpus conformance harness (`parser_test.go`); the public
  Parse API and AST arrive in milestone 3.

## Rules of the port

- **Effective behavior over textbook behavior.** darkwing replicates what the
  pinned DuckDB build actually does, including acknowledged quirks: negative
  lookahead (`!`) is parsed but ignored by the matcher; `/` binds only the
  immediately preceding element (upstream grammars parenthesize sequences in
  alternatives); `*`/`+`/`?` wrap only the last element of the current list.
  Do not "fix" these — they are pinned by tests.
- **Vendored files are verbatim.** `internal/grammar/duckdb/**` is copied
  unmodified from upstream. Changing grammar behavior means advancing the pin,
  never editing the files.
- **Zero dependencies.** `go.mod` stays module line + Go version only.

## Dev loop

```
go build ./... && go vet ./... && gofmt -l . && go test -race ./...
```

Debugging a parse:

```
go run ./cmd/debug-parse 'SELECT 1'            # ParseResult tree
go run ./cmd/debug-parse -tokens 'SELECT 1'    # token dump
```

## Conformance loop

The milestone-2 gate: darkwing accepts a statement iff the pinned DuckDB
binary parses it, over the whole corpus (`go test ./parser`).

```
go run ./cmd/next-test                                  # pick the next todo case
go test ./parser -run TestCorpus -check-parse 'FRAGMENT' # dump detail for it
# ...fix the engine...
go test ./parser                                        # gate
```

Some oracle rejects come from upstream's *transformer* (Parser Errors whose
message is not "syntax error at or near ..."): the matcher alone cannot
reject those, so they stay in todo metadata until the matching transformer
lands (milestones 3-5). Todo entries carry a note saying why.

Regenerating the corpus (needs a DuckDB checkout at the pinned commit and
the matching nightly CLI; see `internal/grammar/README.md` for the pin):

```
DARKWING_DUCKDB=/path/to/duckdb-cli \
  go run ./cmd/regenerate -duckdb-src /path/to/duckdb
```

Never commit binaries (the CLI, *.db files) — only source and corpus text.

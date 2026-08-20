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

Corpus tooling (`cmd/next-test`, `cmd/regenerate`, `-check-parse`) arrives in
milestone 2; until then upstream-transcribed unit tests are the safety net.

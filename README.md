# darkwing

*The terror that flaps in the night, for DuckDB.*

darkwing is a hand-written, zero-dependency Go port of DuckDB v2.0's
PEG-based SQL parser, built to power a first-class DuckDB engine in
[sqlc](https://github.com/sqlc-dev/sqlc). See [PLAN.md](PLAN.md) for the full
design.

Unlike its siblings ([oliphant](https://github.com/sqlc-dev/oliphant),
[marino](https://github.com/sqlc-dev/marino),
[meyer](https://github.com/sqlc-dev/meyer),
[teesql](https://github.com/sqlc-dev/teesql),
[zetajones](https://github.com/sqlc-dev/zetajones),
[doubleclick](https://github.com/sqlc-dev/doubleclick)), darkwing does not
re-express the grammar as recursive descent: DuckDB's own production parser
is an interpreter over machine-readable grammar text, so darkwing vendors the
`.gram`/`.list` files verbatim and ports the engine — tokenizer, grammar
loader, and matcher (with packrat memoization) — to Go.

## Status

Milestone 1 (engine) is complete:

- `token/` — token kinds and DuckDB's keyword categories
- `lexer/` — port of the parsing tokenizer
- `internal/grammar/` — vendored grammar (pinned upstream commit recorded in
  `internal/grammar/README.md`) and the grammar loader
- `internal/matcher/` — matcher tree, packrat memoization, rule overrides,
  furthest-failure error reporting
- `cmd/debug-parse` — dump tokens or the raw parse tree for SQL on the
  command line

Next: corpus extraction and the accept/reject conformance gate against a
pinned DuckDB binary (milestone 2), then the AST and transformers.

```
$ go run ./cmd/debug-parse 'SELECT 1'
$ go run ./cmd/debug-parse -tokens 'SELECT * FROM t'
```

## License

MIT (see `LICENSE`). Vendored DuckDB grammar files and the ported engine
derive from MIT-licensed DuckDB source; see `LICENSE.DUCKDB`.

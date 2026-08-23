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

Milestones 1–6 are complete: the engine (tokenizer, grammar loader,
matcher with packrat memoization), the corpus conformance gates, the full
typed AST with a transformer for every statement, and the hardening pass.
`go test ./parser` enforces, corpus-wide against the pinned DuckDB CLI:

- **Accept/reject** — darkwing accepts a statement iff the pinned binary
  parses it (`TestCorpus`), including transformer-raised parser errors.
- **Error fidelity** — every rejection renders the oracle's error message
  verbatim (`TestCorpusErrorMessages`), with positions pinned by unit
  tests.
- **Tree shape** — `internal/serialize` output matches vendored
  `json_serialize_sql` goldens for the SELECT subset
  (`TestSerializeGoldens`), with AST snapshots covering the rest.

Hardening: `FuzzParse` holds the public API to its invariants on
arbitrary input, benchmarks pin parse throughput and upstream's
pathological-backtracking case (19 unmatched parens: milliseconds, thanks
to packrat), and `cmd/difftest` mutates corpus seeds and diffs darkwing's
verdicts against the pinned CLI (run nightly in CI while the nightly
artifact matches the pin).

Next: advancing the pin to the v2.0.0 tag when it lands (milestone 7),
then sqlc integration.

```
$ go run ./cmd/debug-parse 'SELECT 1'
$ go run ./cmd/debug-parse -tokens 'SELECT * FROM t'
$ go run ./cmd/debug-parse -ast 'FROM t SELECT x'
```

## License

MIT (see `LICENSE`). Vendored DuckDB grammar files and the ported engine
derive from MIT-licensed DuckDB source; see `LICENSE.DUCKDB`.

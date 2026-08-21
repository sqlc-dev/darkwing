# Serialize goldens (tree-shape oracle)

`goldens.jsonl` holds one JSON object per line: a corpus SQL statement and
the exact `json_serialize_sql` output the pinned DuckDB binary produced
for it. `parser/serialize_test.go` re-serializes each statement through
`internal/serialize` and compares structurally (see the documented
normalizations on `serialize.Equal`).

Never edit this file by hand. It is a deterministic sample of the
corpus's SELECT subset, written by:

```
DARKWING_DUCKDB=/path/to/duckdb-cli \
  go run ./cmd/serialize-diff -corpus -quiet \
  -write-goldens parser/testdata/serialize/goldens.jsonl -sample-every 12
```

Only statements where darkwing already matches the oracle are sampled, so
regenerate goldens *after* the full sweep is clean:

```
DARKWING_DUCKDB=/path/to/duckdb-cli go run ./cmd/serialize-diff -corpus
```

The pinned binary is the one recorded in `internal/grammar/README.md`;
goldens, grammar and corpus expectations must come from the same pin.

At the time of writing the full sweep covers 25,414 SELECT-subset
statements: 25,301 match, 0 mismatch, 113 skipped (statements the pinned
binary itself refuses to serialize, e.g. PIVOT-with-enum-discovery forms
that upstream turns into multi-statement plans).

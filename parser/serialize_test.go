// serialize_test.go: the milestone-3 tree-shape gate. The vendored
// goldens under testdata/serialize hold json_serialize_sql output from
// the pinned DuckDB binary for a sample of the corpus's SELECT subset;
// darkwing's internal/serialize output must match structurally.
//
// The goldens are written by cmd/serialize-diff (-corpus -write-goldens),
// never by hand; the same tool sweeps the full corpus against a live
// oracle binary. The comparator's normalizations are documented on
// serialize.Equal.
package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/serialize"
)

type serializeGolden struct {
	SQL  string          `json:"sql"`
	JSON json.RawMessage `json:"json"`
}

func TestSerializeGoldens(t *testing.T) {
	path := filepath.Join("testdata", "serialize", "goldens.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no serialize goldens at %s (run cmd/serialize-diff -corpus -write-goldens)", path)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var g serializeGolden
		if err := json.Unmarshal(scanner.Bytes(), &g); err != nil {
			t.Fatalf("%s:%d: %v", path, line, err)
		}
		stmts, err := Parse(context.Background(), strings.NewReader(g.SQL))
		if err != nil {
			t.Errorf("%s:%d: parse failed: %v\nSQL: %s", path, line, err, g.SQL)
			continue
		}
		got, err := serialize.Script(stmts)
		if err != nil {
			t.Errorf("%s:%d: serialize failed: %v\nSQL: %s", path, line, err, g.SQL)
			continue
		}
		equal, diff, err := serialize.Equal(g.JSON, got)
		if err != nil {
			t.Fatalf("%s:%d: %v", path, line, err)
		}
		if !equal {
			t.Errorf("%s:%d: tree mismatch at %s\nSQL: %s", path, line, diff, g.SQL)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if line == 0 {
		t.Fatalf("%s is empty", path)
	}
}

// Command serialize-diff compares darkwing's json_serialize_sql-compatible
// output (internal/serialize) against the pinned DuckDB CLI, statement by
// statement — the milestone-3 tree-shape conformance loop.
//
// Usage:
//
//	serialize-diff 'SELECT 1' ...          # diff specific statements
//	serialize-diff -corpus [-limit N]      # sweep the SELECT subset of the corpus
//	serialize-diff -corpus -quiet          # counts only
//
// With -write-goldens, matching statements are sampled into the vendored
// golden file consumed by parser/serialize_test.go:
//
//	serialize-diff -corpus -write-goldens parser/testdata/serialize/goldens.jsonl
//
// The oracle binary is located via $DARKWING_DUCKDB (see internal/duckdbsrc).
// Comparison is structural (parsed JSON) via internal/serialize.Equal,
// which documents the normalizations for upstream's unordered containers.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sqlc-dev/darkwing/ast"
	"github.com/sqlc-dev/darkwing/internal/duckdbsrc"
	"github.com/sqlc-dev/darkwing/internal/serialize"
	"github.com/sqlc-dev/darkwing/internal/testfile"
	"github.com/sqlc-dev/darkwing/parser"
)

func main() {
	corpus := flag.Bool("corpus", false, "sweep the SELECT subset of parser/testdata/corpus")
	limit := flag.Int("limit", 0, "max corpus statements to check (0 = all)")
	quiet := flag.Bool("quiet", false, "print only the summary")
	maxPrint := flag.Int("max-print", 10, "max mismatches to print in detail")
	verbose := flag.Bool("v", false, "print both JSON trees for each mismatch")
	writeGoldens := flag.String("write-goldens", "", "write sampled matching statements to this JSONL golden file")
	sampleEvery := flag.Int("sample-every", 20, "with -write-goldens, keep every n-th matching statement")
	flag.Parse()

	bin, err := duckdbsrc.Find()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var sqls []string
	if *corpus {
		sqls, err = corpusSelects(*limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else {
		sqls = flag.Args()
	}
	if len(sqls) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to check")
		os.Exit(2)
	}

	oracle, err := oracleSerialize(bin, sqls)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var match, mismatch, skipped, printed int
	var goldens []goldenEntry
	for i, sql := range sqls {
		want := oracle[i]
		got, ok := darkwingSerialize(sql)
		if !ok {
			skipped++
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(want), &probe); err != nil {
			skipped++
			continue
		}
		if probe["error"] == true {
			// the oracle refused to serialize (non-SELECT reached only via
			// transformer desugaring)
			skipped++
			continue
		}
		equal, diff, err := serialize.Equal([]byte(want), got)
		if err != nil {
			panic(err)
		}
		if equal {
			match++
			if *writeGoldens != "" && match%*sampleEvery == 0 {
				goldens = append(goldens, goldenEntry{SQL: sql, JSON: json.RawMessage(want)})
			}
			continue
		}
		mismatch++
		if !*quiet && printed < *maxPrint {
			printed++
			fmt.Printf("== MISMATCH: %s\n", sql)
			fmt.Printf("   path: %s\n", diff)
			if *verbose {
				fmt.Printf("-- want:\n%s\n-- got:\n%s\n", indentJSON(want), indentJSON(string(got)))
			}
		}
	}
	fmt.Printf("checked %d: %d match, %d mismatch, %d skipped\n",
		match+mismatch+skipped, match, mismatch, skipped)
	if *writeGoldens != "" {
		if err := writeGoldenFile(*writeGoldens, goldens); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %d goldens to %s\n", len(goldens), *writeGoldens)
	}
	if mismatch > 0 {
		os.Exit(1)
	}
}

type goldenEntry struct {
	SQL  string          `json:"sql"`
	JSON json.RawMessage `json:"json"`
}

func writeGoldenFile(path string, entries []goldenEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	return w.Flush()
}

func indentJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

// darkwingSerialize parses and serializes; ok is false when the statement
// is not a serializable SELECT (or parsing fails — the accept/reject gate
// owns those).
func darkwingSerialize(sql string) ([]byte, bool) {
	defer func() { recover() }()
	stmts, err := parser.Parse(context.Background(), strings.NewReader(sql))
	if err != nil || len(stmts) == 0 {
		return nil, false
	}
	for _, s := range stmts {
		if _, ok := s.(*ast.SelectStatement); !ok {
			return nil, false
		}
	}
	out, err := serialize.Script(stmts)
	if err != nil {
		return nil, false
	}
	return out, true
}

// corpusSelects extracts the must-accept statements that darkwing parses
// into SELECT statements.
func corpusSelects(limit int) ([]string, error) {
	var out []string
	root := filepath.Join("parser", "testdata", "corpus")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".test") {
			return err
		}
		if limit > 0 && len(out) >= limit {
			return fs.SkipAll
		}
		file, err := testfile.Read(path)
		if err != nil {
			return err
		}
		for i := range file.Cases {
			c := &file.Cases[i]
			if c.Reject {
				continue
			}
			if isSelectOnly(c.SQL) {
				out = append(out, c.SQL)
				if limit > 0 && len(out) >= limit {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return out, err
}

func isSelectOnly(sql string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	stmts, err := parser.Parse(context.Background(), strings.NewReader(sql))
	if err != nil || len(stmts) == 0 {
		return false
	}
	for _, s := range stmts {
		if _, isSel := s.(*ast.SelectStatement); !isSel {
			return false
		}
	}
	return true
}

// oracleSerialize runs json_serialize_sql over every statement in one CLI
// invocation, returning raw JSON per input.
func oracleSerialize(bin string, sqls []string) ([]string, error) {
	var script bytes.Buffer
	script.WriteString(".mode line\n")
	for _, sql := range sqls {
		escaped := strings.ReplaceAll(sql, "'", "''")
		fmt.Fprintf(&script, "SELECT json_serialize_sql('%s') AS j;\n", escaped)
	}
	cmd := exec.Command(bin, "-batch", "-noheader", "-list", ":memory:")
	cmd.Stdin = &script
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("oracle run: %w", err)
	}
	var results []string
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "j = ")
		if line == "" || line == "j" {
			continue
		}
		results = append(results, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(results) != len(sqls) {
		return nil, errors.New("oracle produced " + fmt.Sprint(len(results)) + " results for " + fmt.Sprint(len(sqls)) + " statements")
	}
	return results, nil
}

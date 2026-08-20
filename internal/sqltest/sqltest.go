// Package sqltest extracts SQL statements from sqllogictest .test files in
// DuckDB's dialect (test/sql/**/*.test and test/fuzzer/**). It is a small,
// tolerant reader: it understands just enough of the format to find the SQL
// blocks, and keeps every statement literal — loop/foreach template
// substitutions are never performed, and statements that contain
// substitution syntax are skipped.
package sqltest

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// Statement is one SQL statement extracted from a .test file.
type Statement struct {
	SQL string
	// Line is the 1-based line number of the first SQL line.
	Line int
}

// directivePrefixes start a record whose body is SQL.
var sqlDirectives = []string{
	"statement ",
	"query",
}

// conditionPrefixes precede a record and are skipped.
var conditionPrefixes = []string{
	"skipif ",
	"onlyif ",
}

func hasSQLDirective(line string) bool {
	for _, p := range sqlDirectives {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	// bare "query" with no type string
	return line == "query"
}

func isCondition(line string) bool {
	for _, p := range conditionPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// ExtractFile extracts the statements of one .test file.
func ExtractFile(path string) ([]Statement, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Extract(f)
}

// Extract reads sqllogictest input and returns the SQL statements in
// order. Statements containing template substitutions (${...}) are
// skipped; all other statements are returned exactly as written.
func Extract(r io.Reader) ([]Statement, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var statements []Statement
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if isCondition(trimmed) {
			continue
		}
		if !hasSQLDirective(trimmed) {
			// some other directive (require, mode, loop, restart, set,
			// sleep, load, ...) - not a SQL record
			continue
		}
		// SQL block: lines until an empty-or-comment line or the ----
		// separator. This matches upstream's SQLLogicParser: a record ends
		// at `line.empty() || StartsWith(line, "#")` (after \r stripping),
		// so a whitespace-only line does NOT end the record, and a comment
		// line does.
		var sql []string
		start := 0
		sawSeparator := false
		for scanner.Scan() {
			lineNo++
			body := strings.ReplaceAll(scanner.Text(), "\r", "")
			if body == "----" {
				sawSeparator = true
				break
			}
			if body == "" || strings.HasPrefix(body, "#") {
				break
			}
			if start == 0 {
				start = lineNo
			}
			sql = append(sql, body)
		}
		if sawSeparator {
			// skim the expected results / error text until the
			// record-ending line, so result rows are never mistaken for
			// directives
			for scanner.Scan() {
				lineNo++
				body := strings.ReplaceAll(scanner.Text(), "\r", "")
				if body == "" || strings.HasPrefix(body, "#") {
					break
				}
			}
		}
		if len(sql) == 0 {
			continue
		}
		text := strings.Join(sql, "\n")
		if strings.Contains(text, "${") {
			// loop/foreach template substitution - skipped, never expanded
			continue
		}
		statements = append(statements, Statement{SQL: text, Line: start})
	}
	return statements, scanner.Err()
}

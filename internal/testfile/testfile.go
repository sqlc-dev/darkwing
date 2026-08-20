// Package testfile reads and writes darkwing's consolidated corpus format:
// one corpus file per upstream .test file, cases separated by a line
// containing only "==", the SQL and its expectation separated by a line
// containing only "--" (the meyer-style storage from PLAN.md). Expectations
// are derived from the pinned DuckDB binary by cmd/regenerate and are never
// edited by hand.
//
// A per-file JSON sidecar (<file>.metadata.json) carries todo/skip lists,
// keyed by the case's content hash so entries survive corpus regeneration.
package testfile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	caseSeparator = "=="
	partSeparator = "--"
)

// Case is one corpus entry: a statement and the pinned oracle's verdict.
type Case struct {
	SQL string
	// Reject is true when the pinned DuckDB binary raised a Parser Error
	// for this statement. Success and post-parse errors (Binder, Catalog,
	// ...) are both must-accept.
	Reject bool
	// Error is the first line of the oracle's error message for rejected
	// statements ("Parser Error: ...").
	Error string
	// Index is the 0-based position within the file.
	Index int
}

// Key returns the case's content hash: the stable identity used by
// metadata sidecars.
func (c *Case) Key() string {
	return CaseKey(c.SQL)
}

// CaseKey hashes a statement's text into a short stable identifier.
func CaseKey(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:8])
}

// File is one corpus file.
type File struct {
	// Header lines are written as leading '#' comments (provenance).
	Header []string
	Cases  []Case
}

// CanStore reports whether a statement can be represented in the corpus
// format: the format delimits with whole lines "==" and "--", so a
// statement containing either as an exact line cannot be stored.
func CanStore(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if line == caseSeparator || line == partSeparator {
			return false
		}
	}
	return true
}

// Read parses a corpus file.
func Read(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	file := &File{}
	var sql []string
	var expectation []string
	inExpectation := false
	sawContent := false
	lineNo := 0

	flush := func() error {
		if !sawContent {
			return nil
		}
		if !inExpectation {
			return fmt.Errorf("%s: case %d has no expectation (missing %q separator)",
				path, len(file.Cases), partSeparator)
		}
		c := Case{SQL: strings.Join(sql, "\n"), Index: len(file.Cases)}
		if len(expectation) == 0 {
			return fmt.Errorf("%s: case %d has an empty expectation", path, len(file.Cases))
		}
		verdict := expectation[0]
		switch {
		case verdict == "ok":
			// must-accept
		case strings.HasPrefix(verdict, "error: "):
			c.Reject = true
			c.Error = strings.TrimPrefix(verdict, "error: ")
		default:
			return fmt.Errorf("%s: case %d has an unrecognized expectation %q", path, len(file.Cases), verdict)
		}
		file.Cases = append(file.Cases, c)
		sql, expectation = nil, nil
		inExpectation = false
		sawContent = false
		return nil
	}

	headerDone := false
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if !headerDone {
			if strings.HasPrefix(line, "#") {
				file.Header = append(file.Header, strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "))
				continue
			}
			if line == "" {
				continue
			}
			headerDone = true
		}
		switch line {
		case caseSeparator:
			if err := flush(); err != nil {
				return nil, err
			}
		case partSeparator:
			if inExpectation {
				return nil, fmt.Errorf("%s:%d: duplicate %q separator", path, lineNo, partSeparator)
			}
			inExpectation = true
			sawContent = true
		default:
			if line == "" && !sawContent {
				// stray blank line between cases
				continue
			}
			sawContent = true
			if inExpectation {
				expectation = append(expectation, line)
			} else {
				sql = append(sql, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return file, nil
}

// Write renders the corpus file.
func Write(path string, file *File) error {
	var sb strings.Builder
	for _, h := range file.Header {
		sb.WriteString("# ")
		sb.WriteString(h)
		sb.WriteString("\n")
	}
	if len(file.Header) > 0 {
		sb.WriteString("\n")
	}
	for i := range file.Cases {
		c := &file.Cases[i]
		sb.WriteString(c.SQL)
		sb.WriteString("\n")
		sb.WriteString(partSeparator)
		sb.WriteString("\n")
		if c.Reject {
			sb.WriteString("error: ")
			sb.WriteString(c.Error)
		} else {
			sb.WriteString("ok")
		}
		sb.WriteString("\n")
		sb.WriteString(caseSeparator)
		sb.WriteString("\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// Metadata is the per-file todo/skip sidecar. Keys are case content
// hashes (Case.Key); values are free-form notes on why the case is
// deferred or excluded.
type Metadata struct {
	// Todo cases are expected to disagree with the oracle for now: the
	// harness inverts them (a todo case that starts passing must be
	// removed from the list).
	Todo map[string]string `json:"todo,omitempty"`
	// Skip cases are not run at all (e.g. statements that hit engine
	// limits unrelated to conformance).
	Skip map[string]string `json:"skip,omitempty"`
}

// MetadataPath returns the sidecar path for a corpus file.
func MetadataPath(corpusPath string) string {
	return corpusPath + ".metadata.json"
}

// ReadMetadata loads the sidecar; a missing sidecar is an empty Metadata.
func ReadMetadata(corpusPath string) (*Metadata, error) {
	data, err := os.ReadFile(MetadataPath(corpusPath))
	if os.IsNotExist(err) {
		return &Metadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", MetadataPath(corpusPath), err)
	}
	return &m, nil
}

// WriteMetadata saves the sidecar, deleting it when empty.
func WriteMetadata(corpusPath string, m *Metadata) error {
	path := MetadataPath(corpusPath)
	if len(m.Todo) == 0 && len(m.Skip) == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// SortKeys returns the map's keys in stable order (for deterministic
// reporting).
func SortKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

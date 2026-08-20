// Package grammar embeds the DuckDB PEG grammar vendored from the pinned
// upstream commit and loads it into rule objects. It is a port of DuckDB's
// src/parser/peg/peg_parser.cpp and parsed_grammar.cpp, plus the grammar
// assembly performed upstream by scripts/parser/inline_grammar.py.
package grammar

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed duckdb/statements/*.gram duckdb/keywords/*.list
var files embed.FS

// KeywordList names one vendored keyword .list file.
type KeywordList struct {
	// File is the vendored file name, e.g. "reserved_keyword.list".
	File string
	// RuleName is the grammar rule the list becomes when the grammar is
	// assembled, e.g. "ReservedKeyword" (upstream: filename_to_upper_camel).
	RuleName string
}

// KeywordLists enumerates the five vendored keyword lists in the order the
// assembly step visits them (sorted by file name, as upstream does).
func KeywordLists() []KeywordList {
	return []KeywordList{
		{File: "column_name_keyword.list", RuleName: "ColumnNameKeyword"},
		{File: "func_name_keyword.list", RuleName: "FuncNameKeyword"},
		{File: "reserved_keyword.list", RuleName: "ReservedKeyword"},
		{File: "type_name_keyword.list", RuleName: "TypeNameKeyword"},
		{File: "unreserved_keyword.list", RuleName: "UnreservedKeyword"},
	}
}

// Keywords returns the keywords of one vendored list, lowercased, in file
// order (upstream lowercases on load; the lists are already lower-case).
func Keywords(list string) ([]string, error) {
	data, err := files.ReadFile("duckdb/keywords/" + list)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		result = append(result, strings.ToLower(line))
	}
	return result, nil
}

// GrammarFiles returns the vendored .gram file names, sorted.
func GrammarFiles() ([]string, error) {
	entries, err := files.ReadDir("duckdb/statements")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".gram") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// AssembledText builds the complete grammar text the same way upstream's
// inline_grammar.py does: common.gram first, then each keyword list rendered
// as an ordered-choice rule of quoted keywords, then the remaining .gram
// files in sorted order.
func AssembledText() (string, error) {
	var sb strings.Builder

	common, err := files.ReadFile("duckdb/statements/common.gram")
	if err != nil {
		return "", err
	}
	sb.Write(common)
	sb.WriteString("\n")

	for _, list := range KeywordLists() {
		keywords, err := Keywords(list.File)
		if err != nil {
			return "", err
		}
		sb.WriteString(list.RuleName)
		sb.WriteString(" <- ")
		for i, kw := range keywords {
			if i > 0 {
				sb.WriteString(" /\n")
			}
			sb.WriteString("'")
			sb.WriteString(kw)
			sb.WriteString("'")
		}
		sb.WriteString("\n")
	}

	names, err := GrammarFiles()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		if name == "common.gram" {
			continue
		}
		data, err := files.ReadFile("duckdb/statements/" + name)
		if err != nil {
			return "", err
		}
		sb.Write(data)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

var (
	loadOnce sync.Once
	loaded   *Grammar
	loadErr  error
)

// Load parses the embedded grammar once and returns the immutable result.
// The returned Grammar is safe for concurrent use.
func Load() (*Grammar, error) {
	loadOnce.Do(func() {
		text, err := AssembledText()
		if err != nil {
			loadErr = err
			return
		}
		g, err := Parse(text)
		if err != nil {
			loadErr = fmt.Errorf("parsing embedded grammar: %w", err)
			return
		}
		// Upstream's ParsedGrammar::CreateDefault adds an empty EndOfInput
		// rule when the grammar text does not define one; the matcher
		// satisfies it with a rule override.
		if g.Rule("EndOfInput") == nil {
			g.addRule(&Rule{Name: "EndOfInput"})
		}
		loaded = g
	})
	return loaded, loadErr
}

package grammar

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// EnvDuckDBSource names a DuckDB checkout at the pinned commit. When set,
// TestAssembledTextMatchesUpstream compares AssembledText with the
// grammar upstream actually compiles.
const EnvDuckDBSource = "DARKWING_DUCKDB_SRC"

// AssembledText must reproduce upstream's inline_grammar.py output, the
// INLINED_PEG_GRAMMAR byte array of inlined_grammar.hpp that
// ParsedGrammar::CreateDefault parses. The comparison ignores blank lines
// and case: rule names and literals are both matched case-insensitively,
// and darkwing lowercases the keyword lists on load. Opt-in, since the
// header is not vendored.
func TestAssembledTextMatchesUpstream(t *testing.T) {
	src := os.Getenv(EnvDuckDBSource)
	if src == "" {
		t.Skipf("%s not set", EnvDuckDBSource)
	}
	header, err := os.ReadFile(filepath.Join(src, "src/include/duckdb/parser/peg/inlined_grammar.hpp"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(header)
	start := strings.Index(text, "INLINED_PEG_GRAMMAR[] = {")
	end := strings.Index(text, "};")
	if start < 0 || end < start {
		t.Fatal("INLINED_PEG_GRAMMAR array not found")
	}
	var upstream []byte
	for _, digits := range regexp.MustCompile(`\d+`).FindAllString(text[start+len("INLINED_PEG_GRAMMAR[] = {"):end], -1) {
		b, err := strconv.Atoi(digits)
		if err != nil || b > 255 {
			t.Fatalf("bad byte %q", digits)
		}
		if b != 0 {
			upstream = append(upstream, byte(b))
		}
	}
	assembled, err := AssembledText()
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(s string) []string {
		var lines []string
		for _, line := range strings.Split(s, "\n") {
			if strings.TrimSpace(line) != "" {
				lines = append(lines, strings.ToLower(line))
			}
		}
		return lines
	}
	want, got := normalize(string(upstream)), normalize(assembled)
	if len(want) < 100 {
		t.Fatalf("upstream grammar has only %d lines", len(want))
	}
	for i := 0; i < len(want) && i < len(got); i++ {
		if want[i] != got[i] {
			t.Fatalf("line %d differs:\nupstream:  %s\nassembled: %s", i+1, want[i], got[i])
		}
	}
	if len(want) != len(got) {
		t.Fatalf("assembled grammar has %d non-blank lines, upstream %d", len(got), len(want))
	}
}

package token

import (
	"sort"
	"strings"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/grammar"
)

// Per-list keyword counts at the current pin. A pin advance that changes
// these is expected to update this test alongside the vendored lists.
func TestKeywordListCounts(t *testing.T) {
	counts := map[string]int{
		"bare_label_keyword.list":  358,
		"reserved_keyword.list":    75,
		"unreserved_keyword.list":  342,
		"column_name_keyword.list": 55,
		"func_name_keyword.list":   30,
		"type_name_keyword.list":   32,
	}
	total := 0
	for _, list := range grammar.KeywordLists() {
		words, err := grammar.Keywords(list.File)
		if err != nil {
			t.Fatalf("Keywords(%s): %v", list.File, err)
		}
		if got, want := len(words), counts[list.File]; got != want {
			t.Errorf("%s: %d keywords, want %d", list.File, got, want)
		}
		total += len(words)
	}
	if total != 892 {
		t.Errorf("total keyword entries = %d, want 892", total)
	}
}

func TestCategoryCounts(t *testing.T) {
	for _, tc := range []struct {
		category KeywordCategory
		want     int
	}{
		{KeywordReserved, 75},
		{KeywordUnreserved, 342},
		{KeywordColumnName, 55},
		{KeywordFuncName, 30},
		{KeywordTypeName, 32},
		{KeywordBareLabel, 358},
		// the bare-label list adds no new words
		{KeywordNone, 502},
	} {
		if got := KeywordCount(tc.category); got != tc.want {
			t.Errorf("KeywordCount(%s) = %d, want %d", tc.category, got, tc.want)
		}
	}
}

// Upstream's build script enforces only that no keyword is both reserved
// and unreserved; the other lists overlap. Pin the exact overlap set so a
// pin advance surfaces membership changes.
func TestReservedUnreservedDisjoint(t *testing.T) {
	reserved, err := grammar.Keywords("reserved_keyword.list")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range reserved {
		if Categories(w).Has(KeywordUnreserved) {
			t.Errorf("keyword %q is both reserved and unreserved", w)
		}
	}
}

func TestMultiCategoryKeywords(t *testing.T) {
	single := func(c KeywordCategory) bool {
		return c&(c-1) == 0
	}
	var multi []string
	lists := make(map[string][]string)
	for _, list := range grammar.KeywordLists() {
		words, err := grammar.Keywords(list.File)
		if err != nil {
			t.Fatal(err)
		}
		lists[list.File] = words
	}
	seen := map[string]bool{}
	for _, words := range lists {
		for _, w := range words {
			// the bare-label classification is orthogonal to the other
			// five; count overlaps among those
			if !seen[w] && !single(Categories(w)&^KeywordBareLabel) {
				multi = append(multi, w)
			}
			seen[w] = true
		}
	}
	sort.Strings(multi)
	// 32 keywords appear in two lists at the current pin
	if len(multi) != 32 {
		t.Errorf("multi-category keywords = %d (%v), want 32", len(multi), multi)
	}
	// spot-check the documented examples
	if got := Categories("generated"); got != KeywordColumnName|KeywordFuncName|KeywordBareLabel {
		t.Errorf("Categories(generated) = %s", got)
	}
	if got := Categories("map"); got != KeywordColumnName|KeywordFuncName|KeywordBareLabel {
		t.Errorf("Categories(map) = %s", got)
	}
	if got := Categories("is"); got != KeywordFuncName|KeywordTypeName {
		t.Errorf("Categories(is) = %s", got)
	}
}

// Every bare-label keyword is also an unreserved or column-name keyword
// (upstream's build script enforces it); the converse does not hold. Pin
// the unreserved/column-name keywords that are not bare labels, the ones a
// column alias without AS cannot use.
func TestBareLabelKeywords(t *testing.T) {
	var notBare []string
	for _, list := range []string{"unreserved_keyword.list", "column_name_keyword.list"} {
		words, err := grammar.Keywords(list)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range words {
			if !Categories(w).Has(KeywordBareLabel) {
				notBare = append(notBare, w)
			}
		}
	}
	sort.Strings(notBare)
	want := []string{
		"centuries", "century", "day", "days", "decade", "decades", "escape",
		"exclude", "export_state", "filter", "hour", "hours", "ignore",
		"microsecond", "microseconds", "millennia", "millennium",
		"millisecond", "milliseconds", "minute", "minutes", "month",
		"months", "over", "precision", "quarter", "quarters", "rename",
		"replace", "respect", "second", "seconds", "varying", "week", "weeks",
		"within", "without", "year", "years",
	}
	if strings.Join(notBare, " ") != strings.Join(want, " ") {
		t.Errorf("unreserved/column-name keywords that are not bare labels = %v, want %v", notBare, want)
	}
	for _, w := range []string{"abort", "between", "destroy", "secure"} {
		if !Categories(w).Has(KeywordBareLabel) {
			t.Errorf("Categories(%s) = %s, want bare_label", w, Categories(w))
		}
	}
	if Categories("select").Has(KeywordBareLabel) {
		t.Error("reserved keyword select is a bare label")
	}
}

func TestCategoriesCaseInsensitive(t *testing.T) {
	for _, w := range []string{"select", "SELECT", "Select", "sElEcT"} {
		if got := Categories(w); !got.Has(KeywordReserved) {
			t.Errorf("Categories(%q) = %s, want reserved", w, got)
		}
	}
	if IsKeyword("definitely_not_a_keyword") {
		t.Error("IsKeyword(definitely_not_a_keyword) = true")
	}
	if !IsKeyword("abort") {
		t.Error("IsKeyword(abort) = false, want true (unreserved)")
	}
}

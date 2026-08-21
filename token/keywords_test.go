package token

import (
	"sort"
	"testing"

	"github.com/sqlc-dev/darkwing/internal/grammar"
)

// Per-list keyword counts at the current pin. A pin advance that changes
// these is expected to update this test alongside the vendored lists.
func TestKeywordListCounts(t *testing.T) {
	counts := map[string]int{
		"reserved_keyword.list":    75,
		"unreserved_keyword.list":  338,
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
	if total != 530 {
		t.Errorf("total keyword entries = %d, want 530", total)
	}
}

func TestCategoryCounts(t *testing.T) {
	for _, tc := range []struct {
		category KeywordCategory
		want     int
	}{
		{KeywordReserved, 75},
		{KeywordUnreserved, 338},
		{KeywordColumnName, 55},
		{KeywordFuncName, 30},
		{KeywordTypeName, 32},
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
			if !seen[w] && !single(Categories(w)) {
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
	if got := Categories("generated"); got != KeywordColumnName|KeywordFuncName {
		t.Errorf("Categories(generated) = %s", got)
	}
	if got := Categories("map"); got != KeywordColumnName|KeywordFuncName {
		t.Errorf("Categories(map) = %s", got)
	}
	if got := Categories("is"); got != KeywordFuncName|KeywordTypeName {
		t.Errorf("Categories(is) = %s", got)
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

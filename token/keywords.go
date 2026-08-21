package token

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sqlc-dev/darkwing/internal/grammar"
)

// KeywordCategory is a bitmask of DuckDB keyword categories, one bit per
// vendored .list file. Upstream's build script enforces that a keyword is
// never both reserved and unreserved, but the remaining lists overlap (32
// keywords appear in two lists at the current pin, e.g. GENERATED is both
// a column-name and a func-name keyword), so membership is a set, not a
// single category. The categories decide which identifier positions may
// consume the keyword (PEGKeywordCategory in
// duckdb/parser/peg/keyword_helper.hpp).
type KeywordCategory uint8

const (
	// KeywordReserved keywords can never be used as bare identifiers.
	KeywordReserved KeywordCategory = 1 << iota
	// KeywordUnreserved keywords are allowed in any identifier position.
	KeywordUnreserved
	// KeywordColumnName keywords are allowed as column names.
	KeywordColumnName
	// KeywordFuncName keywords are allowed as function names.
	KeywordFuncName
	// KeywordTypeName keywords are allowed as type names and, like
	// func-name keywords, as function names (upstream folds both lists
	// into its type/func map).
	KeywordTypeName

	// KeywordNone means the word is not a keyword.
	KeywordNone KeywordCategory = 0
)

// Has reports whether c includes category.
func (c KeywordCategory) Has(category KeywordCategory) bool {
	return c&category != 0
}

func (c KeywordCategory) String() string {
	if c == KeywordNone {
		return "none"
	}
	var parts []string
	for _, entry := range []struct {
		bit  KeywordCategory
		name string
	}{
		{KeywordReserved, "reserved"},
		{KeywordUnreserved, "unreserved"},
		{KeywordColumnName, "column_name"},
		{KeywordFuncName, "func_name"},
		{KeywordTypeName, "type_name"},
	} {
		if c.Has(entry.bit) {
			parts = append(parts, entry.name)
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("KeywordCategory(%d)", uint8(c))
	}
	return strings.Join(parts, "|")
}

var (
	keywordOnce sync.Once
	keywordMap  map[string]KeywordCategory
	keywordErr  error
)

func listCategory(file string) KeywordCategory {
	switch file {
	case "reserved_keyword.list":
		return KeywordReserved
	case "unreserved_keyword.list":
		return KeywordUnreserved
	case "column_name_keyword.list":
		return KeywordColumnName
	case "func_name_keyword.list":
		return KeywordFuncName
	case "type_name_keyword.list":
		return KeywordTypeName
	default:
		return KeywordNone
	}
}

func keywords() map[string]KeywordCategory {
	keywordOnce.Do(func() {
		m := make(map[string]KeywordCategory)
		for _, list := range grammar.KeywordLists() {
			words, err := grammar.Keywords(list.File)
			if err != nil {
				keywordErr = err
				return
			}
			category := listCategory(list.File)
			for _, w := range words {
				m[w] |= category
			}
		}
		// the only disjointness upstream's build script enforces
		for w, c := range m {
			if c.Has(KeywordReserved) && c.Has(KeywordUnreserved) {
				keywordErr = fmt.Errorf("keyword %q has conflicting RESERVED/UNRESERVED categories", w)
				return
			}
		}
		keywordMap = m
	})
	if keywordErr != nil {
		panic(keywordErr)
	}
	return keywordMap
}

// Categories returns the category set of word (case-insensitive);
// KeywordNone if word is not a keyword.
func Categories(word string) KeywordCategory {
	return keywords()[strings.ToLower(word)]
}

// IsKeyword reports whether word (case-insensitive) is a keyword in any
// category.
func IsKeyword(word string) bool {
	return Categories(word) != KeywordNone
}

// KeywordCount returns the number of keywords carrying the given category
// bit, or the number of distinct keywords for KeywordNone.
func KeywordCount(category KeywordCategory) int {
	n := 0
	for _, c := range keywords() {
		if category == KeywordNone || c.Has(category) {
			n++
		}
	}
	return n
}

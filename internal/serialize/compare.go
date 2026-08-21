package serialize

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Equal compares two json_serialize_sql documents structurally, applying
// the documented normalizations for fields upstream serializes from
// unordered hash containers:
//
//   - named_param_map entries are sorted by key
//   - exclude_list entries are sorted alphabetically
//   - qualified_exclude_list entries are sorted by their rendered form
//
// On mismatch it returns the path of the first difference.
func Equal(want, got []byte) (bool, string, error) {
	var wantAny, gotAny any
	if err := json.Unmarshal(want, &wantAny); err != nil {
		return false, "", fmt.Errorf("parsing expected JSON: %w", err)
	}
	if err := json.Unmarshal(got, &gotAny); err != nil {
		return false, "", fmt.Errorf("parsing actual JSON: %w", err)
	}
	Normalize(wantAny)
	Normalize(gotAny)
	if reflect.DeepEqual(wantAny, gotAny) {
		return true, "", nil
	}
	return false, FirstDiff(wantAny, gotAny, "$"), nil
}

// Normalize applies the comparator normalizations in place.
func Normalize(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch k {
			case "named_param_map":
				if list, ok := child.([]any); ok {
					sort.Slice(list, func(i, j int) bool {
						a, _ := list[i].(map[string]any)
						b, _ := list[j].(map[string]any)
						ka, _ := a["key"].(string)
						kb, _ := b["key"].(string)
						return ka < kb
					})
				}
			case "exclude_list":
				if list, ok := child.([]any); ok {
					sort.Slice(list, func(i, j int) bool {
						a, _ := list[i].(string)
						b, _ := list[j].(string)
						return a < b
					})
				}
			case "qualified_exclude_list":
				if list, ok := child.([]any); ok {
					sort.Slice(list, func(i, j int) bool {
						a, _ := json.Marshal(list[i])
						b, _ := json.Marshal(list[j])
						return string(a) < string(b)
					})
				}
			}
			Normalize(child)
		}
	case []any:
		for _, child := range t {
			Normalize(child)
		}
	}
}

// FirstDiff walks two parsed JSON trees and reports the first differing
// path, for readable failure messages.
func FirstDiff(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: want object, got %s", path, describeJSON(got))
		}
		var keys []string
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, present := g[k]
			if !present {
				return fmt.Sprintf("%s.%s: missing in actual output", path, k)
			}
			if d := FirstDiff(w[k], gv, path+"."+k); d != "" {
				return d
			}
		}
		for k := range g {
			if _, present := w[k]; !present {
				return fmt.Sprintf("%s.%s: extra in actual output", path, k)
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: want array, got %s", path, describeJSON(got))
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s: want %d elements, got %d", path, len(w), len(g))
		}
		for i := range w {
			if d := FirstDiff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Sprintf("%s: want %s, got %s", path, describeJSON(want), describeJSON(got))
		}
		return ""
	}
}

func describeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 200 {
		b = append(b[:200], []byte("...")...)
	}
	return string(b)
}

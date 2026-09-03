// Package document owns a document's body blob and its derived search index.
// Service.Save is the only code path permitted to write either table.
package document

import (
	"slices"
	"strconv"
	"strings"
)

// FieldText reduces one field value to the plain text stored in search_index.
//
// Both the live write path (Save) and the rebuild path must call this exact
// function. Two extraction implementations would drift the moment one of them
// learned about a field type the other didn't - the precise failure this
// ticket exists to prevent.
//
// Input is always the result of decoding JSON, so this switch is exhaustive.
func FieldText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		// encoding/json decodes every JSON number as float64.
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := FieldText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		// Keys are sorted so output depends only on content, never on Go's
		// randomized map iteration order. Without this, a rebuild could produce
		// different text than the live write for byte-identical input.
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)

		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			if text := FieldText(v[key]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

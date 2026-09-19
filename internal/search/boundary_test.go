package search

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// search_index has one writer, and after LAM-45 it is this package rather than
// internal/document.
//
// internal/document is granted reads, not writes. Save causes index rows through
// ClearDocument and IndexDocument, both of which take its transaction and live here -
// so no search_index SQL exists over there outside tests. Those tests do need to
// observe the rows Save caused: aspect_seam_test.go proves a field uuid survives from
// body key to index row unchanged, which cannot be asserted without reading the index.
//
// Writes are not grantable to anyone. Drift is a write problem.
func TestNoSearchIndexSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "search",
		Tables:  []string{"search_index"},
		Readers: []string{"document"},
	})
}

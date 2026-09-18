package document

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// TestNoSQLOutsideThisPackage fails if any package other than this one issues
// SQL against document or search_index.
//
// Go's encapsulation is package-scoped: Service.pool being unexported stops a
// caller reaching through a Service, but nothing stops a new package calling
// db.Connect and writing the tables itself. That is the hole this closes, and
// docs/adr/0001-write-path-enforcement.md is why.
//
// The walk itself moved to internal/sqlguard under LAM-55, when internal/auth
// needed the same guard for api_token. It is one mechanism with two callers
// rather than two copies of an AST walk - the copy being the thing that drifts,
// since the subtle parts (parse with comments dropped, skip the owner's own
// directory) are invisible when they are wrong.
//
// LAM-45 splits this: search_index moves to internal/search, and this call
// keeps document alone. That is a one-line edit here rather than a rewrite.
func TestNoSQLOutsideThisPackage(t *testing.T) {
	sqlguard.AssertOwned(t, "document", "document", "search_index")
}

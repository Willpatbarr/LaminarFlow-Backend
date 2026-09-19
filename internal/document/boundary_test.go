package document

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Nothing stops a new package calling db.Connect and writing this table itself.
//
// LAM-45 narrowed this to document alone - search_index moved to internal/search - and
// granted that package reads. Its whole job is denormalising workspace_id, project_id
// and title off a document onto the row describing it, so reading document is the
// work, not a bypass. Writes stay owner-only and are not grantable.
func TestNoSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "document",
		Tables:  []string{"document"},
		Readers: []string{"search"},
	})
}

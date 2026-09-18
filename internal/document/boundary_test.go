package document

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Nothing stops a new package calling db.Connect and writing these tables itself.
// The walk moved to internal/sqlguard under LAM-55, when internal/auth needed the
// same guard - LAM-45's split of search_index is then a one-line edit here.
func TestNoSQLOutsideThisPackage(t *testing.T) {
	sqlguard.AssertOwned(t, "document", "document", "search_index")
}

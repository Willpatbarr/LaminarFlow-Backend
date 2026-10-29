package activity

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// The activity table has one writer. A row is only a faithful history if it is written in
// the transaction of the change it describes, which only holds when every write goes through
// Record; an INSERT written elsewhere would be an entry no change backs, or a change no entry
// records. The list is this package's too, so nothing else reads the table either.
func TestNoActivitySQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:  "activity",
		Tables: []string{"activity"},
		// The schema test for 0043 lives in internal/document, beside comment's, because it
		// proves each constraint by violating it and the table points at document.
		Fixtures: []string{"document"},
	})
}

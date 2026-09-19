package ticket

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// The ticket table has one writer, and the scoping and archive predicates only hold
// because every statement goes through this package. A query written elsewhere would
// compile, run, and quietly return other workspaces' tickets - or archived ones.
//
// internal/search reads ticket to denormalise scope and title onto index rows. Reads
// are grantable; writes are not.
func TestNoTicketSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "ticket",
		Tables:  []string{"ticket"},
		Readers: []string{"search"},
		// A comment needs a ticket to hang off; an index row needs one to describe.
		// Those fixtures are setup, and routing them through Create would make every
		// such test build a membership graph to satisfy a predicate it is not
		// testing. Production code in these packages is still guarded.
		Fixtures: []string{"document", "search"},
	})
}

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
		// A comment needs a ticket to hang off; an index row needs one to describe;
		// and internal/account needs an assigned ticket to prove that deleting an
		// account leaves the work and drops only the name. Those fixtures are
		// setup, and routing them through Create would make every such test build a
		// membership graph to satisfy a predicate it is not testing. Production
		// code in these packages is still guarded.
		Fixtures: []string{"account", "document", "migrate", "search"},
	})
}

// ticket_document (LAM-111) is the link between a ticket and a document. internal/ticket owns
// it, though the row names a document too: a link is written against a ticket's reach and
// listed from the ticket's side, and internal/document gets the tickets of a document through
// Service.LinkedTickets rather than SQL of its own. Nobody else reads or writes it.
func TestNoTicketDocumentSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:  "ticket",
		Tables: []string{"ticket_document"},
	})
}

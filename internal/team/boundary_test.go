package team

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Writes only, which is why this rule can exist at all.
//
// LAM-43 shipped with no rule here, on the argument that team is joined by the scoping
// predicate of every service in this codebase - so a Readers list would name most of
// the module and assert nothing. That was right about reads and stopped too early.
//
// The invariant worth protecting was never about reads. Seeding is non-optional only
// because Create is the sole path that makes a team: an INSERT INTO team written
// anywhere else produces a team with no statuses, no starter aspect types and no saved
// view - every foreign key satisfied, nothing erroring, and the first symptom is a
// ticket that cannot be given a status.
//
// sqlguard.AnyReader says "reads unguarded, writes are not" using the field that
// already exists, so the guard keeps its three concepts.
func TestNoTeamSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "team",
		Tables:  []string{"team"},
		Readers: []string{sqlguard.AnyReader},
		// Longer than most fixture lists, and worth stating rather than leaving
		// someone to notice: nearly every scoping assertion in this codebase
		// needs a team to hang a member and an outsider off, so nearly every
		// test package makes one. Six of the seven directories that touch team
		// end up here.
		//
		// What survives is the part that matters. Every exemption is a _test.go,
		// so a production writer in any of them is still caught - which is the
		// half to probe when this list changes.
		Fixtures: []string{"account", "aspect", "document", "search", "setting", "ticket"},
	})
}

package project

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// The same shape as internal/team's rule, one level down and for the same reason:
// project is joined by every scoping predicate, and written by exactly one function.
//
// A project created around Create has no board. That is legal - 0025 says a project
// with no board must render - so this is a weaker invariant than the team one, and it
// is still worth holding: the board is built from the team's statuses, which is
// knowledge seedBoard is the last place to have.
func TestNoProjectSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:    "project",
		Tables:   []string{"project"},
		Readers:  []string{sqlguard.AnyReader},
		Fixtures: []string{"account", "document", "search", "setting", "ticket"},
	})
}

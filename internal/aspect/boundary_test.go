package aspect

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Two tables with one owner, because they are one thing: an aspect type is its fields.
// A field edit written anywhere else would compile, run, and skip the scoping predicate
// that keeps one team's editor out of another team's types.
//
// One reader: internal/search joins aspect_type_field for a field's label, so a search
// hit on a document field can say which field matched (LAM-100). internal/document is
// still not one - it stores aspect_type_field.id as a key inside body and never joins
// to the table to do it, which is what makes the seam a seam.
func TestNoAspectSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "aspect",
		Tables:  []string{"aspect_type", "aspect_type_field"},
		Readers: []string{"search"},
		// internal/document's tests build an aspect type and a field to hang a
		// document off, and read them back to prove the id/body-key seam holds.
		// internal/team's count the types its bootstrap seeded and delete them,
		// to prove a starter type is an ordinary row - which is LAM-21's claim
		// and cannot be asserted through a service that has no delete.
		//
		// Both are assertions about their own package, not editors written
		// elsewhere. Production code in either is guarded exactly as before, and
		// internal/team's own seeding goes through aspect.Bootstrap.
		Fixtures: []string{"document", "migrate", "team"},
	})
}

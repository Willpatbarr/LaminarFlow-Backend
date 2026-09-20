package aspect

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Two tables with one owner, because they are one thing: an aspect type is its fields.
// A field edit written anywhere else would compile, run, and skip the scoping predicate
// that keeps one team's editor out of another team's types.
//
// No readers. Nothing outside this package needs to read either table today -
// internal/document stores aspect_type_field.id as a key inside body and never joins
// to the table to do it, which is what makes the seam a seam.
func TestNoAspectSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:  "aspect",
		Tables: []string{"aspect_type", "aspect_type_field"},
		// internal/document's tests build an aspect type and a field to hang a
		// document off, and read them back to prove the id/body-key seam holds.
		// That is setup and assertion about document, not an editor written
		// elsewhere. Production code there is guarded exactly as before.
		Fixtures: []string{"document"},
	})
}

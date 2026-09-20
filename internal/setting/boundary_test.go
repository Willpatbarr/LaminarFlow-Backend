package setting

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// The registry is only worth having if nothing can write around it. A write from
// another package would store an unregistered key exactly as before - the same silent
// no-op LAM-52 exists to close - so the guard is what turns a Go convention into a
// rule.
//
// No readers. A consumer wanting a setting calls Get, which checks the key against the
// registry on the way through; reading the table directly would skip that and is how a
// reader ends up asking for a key nobody can write.
func TestNoSettingSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:  "setting",
		Tables: []string{"setting"},
		// internal/document's hierarchy test counts a fresh team's settings to
		// prove a team is usable with nothing configured. That is an assertion
		// about the absence of rows, not a second write path.
		Fixtures: []string{"document"},
	})
}

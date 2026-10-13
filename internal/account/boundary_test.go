package account

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// This guard is the point of the package, not decoration around it.
//
// LAM-51's gap is only closed while every account deletion runs the private-view sweep.
// A DELETE FROM account written anywhere else would compile, run, satisfy every foreign
// key, and leave exactly the orphaned rows 0027 describes - with nothing erroring, which
// is what made the gap invisible for two epics.
//
// internal/auth reads account to log someone in. That is a read of the credential
// columns, not a second write path, so it is granted - the same call internal/search
// gets on document and ticket.
func TestNoAccountSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "account",
		Tables:  []string{"account"},
		Readers: []string{"auth"},
		// Longer than the other rules' fixture lists, and for one reason: almost
		// every scoping assertion in this codebase needs a member and an
		// outsider, so almost every test package creates accounts. Each is
		// setup for a test about something else. Production code in all of them
		// is guarded exactly as before, which is what keeps this from being a
		// back door - and is the half worth probing when this rule changes.
		Fixtures: []string{"auth", "aspect", "document", "migrate", "setting", "team", "ticket"},
	})
}

// LAM-61. Registration is one transaction over account, workspace and workspace_member,
// and this package is its only writer: an account with no membership is a login that
// reaches an app with nowhere to go, and nothing in the schema stops one.
//
// Reads are unguarded (AnyReader) for the reason team and project are: every service's
// scoping predicate joins workspace_member, so a reader list would name most of the
// module and assert nothing. The invariant was only ever about writes.
//
// Fixtures is the five packages whose tests create "the other workspace" or a
// membership to prove scoping against, plus api, whose registration tests empty the
// install to reach the no-workspace branch. All are _test.go only; production code in
// any of them is guarded exactly as before.
func TestNoWorkspaceSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:    "account",
		Tables:   []string{"workspace", "workspace_member"},
		Readers:  []string{sqlguard.AnyReader},
		Fixtures: []string{"api", "aspect", "document", "migrate", "setting", "team", "ticket"},
	})
}

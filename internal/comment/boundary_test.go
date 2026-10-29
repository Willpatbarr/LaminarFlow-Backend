package comment

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// comment has one writer. Every write to it must also write search_index in the same
// transaction (LAM-99), which only holds if every write goes through this package: an INSERT
// anywhere else would be a comment search cannot find. comment_reviewer rides with it - a
// reviewer is a decoration of a comment and LAM-113 adds its write path here.
//
// internal/search reads comment to denormalise the parent's scope onto the index row (and its
// rebuild reads them all). Reads are grantable; writes are not.
func TestNoCommentSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:   "comment",
		Tables:  []string{"comment", "comment_reviewer"},
		Readers: []string{"search"},
		// Setup rows in other packages' tests: the schema tests (0017, 0018) in
		// internal/document and internal/migrate, search's rebuild tests, and the API tests
		// that hang a comment off a ticket or a document. Production code there is still guarded.
		Fixtures: []string{"account", "api", "document", "migrate", "search", "ticket"},
	})
}

package document

import (
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// Nothing stops a new package calling db.Connect and writing this table itself.
//
// LAM-45 narrowed this to document alone - search_index moved to internal/search - and
// granted that package reads. Its whole job is denormalising workspace_id, project_id
// and title off a document onto the row describing it, so reading document is the
// work, not a bypass. Writes stay owner-only and are not grantable.
func TestNoSQLOutsideThisPackage(t *testing.T) {
	sqlguard.Assert(t, sqlguard.Rule{
		Owner:  "document",
		Tables: []string{"document"},
		// internal/comment reads document for the same reason it reads ticket (LAM-99): a comment
		// is only written against a live document in the caller's workspace.
		// internal/ticket reads document to link to it (same workspace as the ticket) and to list a
		// ticket's linked documents (LAM-111). Both are reads inside statements that also hold
		// ticket_document, which is why they cannot be a call into this package.
		// internal/attachment (LAM-90) reads document the way comment does, to prove the owner of an
		// upload is live and in the caller's workspace, and to reach its workspace on every read.
		Readers: []string{"attachment", "comment", "search", "ticket"},
		// LAM-44 added internal/aspect, whose entire claim is that a field edit
		// touches no document. Proving that means making a document, editing a
		// field, and reading the document back - from outside, because this guard
		// is what stops internal/aspect doing any of it. Its production code is
		// guarded exactly as before, which is what the test is asserting.
		Fixtures: []string{"aspect"},
	})
}

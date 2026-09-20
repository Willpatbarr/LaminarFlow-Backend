/*
╔═ fields.go ═══════════════════════════════════════════════════════════════════════════
║  ticket · what a caller may filter and sort on
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Fields       filter.Schema
║      Tiebreak     const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  the ticket list body's filter and sort
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import "github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

// Fields is the ticket filter vocabulary, and it lives here rather than in
// internal/filter for two reasons that both point the same way.
//
// It names tables, and sqlguard fails the build on SQL naming ticket outside this
// package - a vocabulary held anywhere else would either trip that guard or force a
// hole in it. And a vocabulary is the part that differs per resource: the next resource
// declares its own beside its own service, which is the same shape as everything else
// in this package.
//
// Every name here is a public API surface twice over. It appears in a request body,
// and saved_view.config persists it, so renaming one is a data migration. Adding one
// is free.
var Fields = filter.Schema{
	"title":       {Expr: "t.title", Kind: filter.Text},
	"description": {Expr: "t.description", Kind: filter.Text},
	"project":     {Expr: "t.project_id", Kind: filter.UUID},
	"status":      {Expr: "t.status_id", Kind: filter.UUID},
	"assignee":    {Expr: "t.assignee_account_id", Kind: filter.UUID},
	"epic":        {Expr: "t.epic_id", Kind: filter.UUID},
	"created_at":  {Expr: "t.created_at", Kind: filter.Time},
	"updated_at":  {Expr: "t.updated_at", Kind: filter.Time},

	// label and sprint are join tables, so neither is a column. As an array-valued
	// subquery each is scalar-shaped to the compiler, which is what lets `and` over
	// two labels and `or` over two labels be one query rather than two query shapes.
	//
	// Both subqueries drive the join table's primary key, which leads with ticket_id -
	// so this is an index lookup per candidate row, not a scan.
	"label":  {Expr: "ARRAY(SELECT tl.label_id FROM ticket_label tl WHERE tl.ticket_id = t.id)", Kind: filter.UUIDSet},
	"sprint": {Expr: "ARRAY(SELECT ts.sprint_id FROM ticket_sprint ts WHERE ts.ticket_id = t.id)", Kind: filter.UUIDSet},
}

// Tiebreak is the last ORDER BY key of every ticket list, and it is a primary key so
// the order it completes is total. Cursor pagination needs that: without it two rows
// sharing a sort value have no fixed sequence, and a cursor between them may skip a
// ticket or hand back one already seen.
const Tiebreak = "t.id"

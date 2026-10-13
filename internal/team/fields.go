/*
╔═ fields.go ═══════════════════════════════════════════════════════════════════════════
║  team · what a caller may filter and sort on
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Fields         filter.Schema
║      Tiebreak       const
║      DefaultSort    var
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  the team list body's filter and sort
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package team

import "github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

// Fields is the team filter vocabulary, declared beside the service for the reason
// ticket.Fields gives: it names a table, and a vocabulary is the part that differs per
// resource (conventions §3).
//
// Every name is persisted public surface, since saved_view.config can hold it. Adding
// one is free; renaming one is a data migration.
//
// updated_at is here for the client that hydrates a local store: "teams changed since
// I last looked" is a filter, not a second endpoint. Statuses are not filterable - they
// ride on the team, and a team's statuses changing does not move its updated_at.
var Fields = filter.Schema{
	"name":       {Expr: "tm.name", Kind: filter.Text},
	"created_at": {Expr: "tm.created_at", Kind: filter.Time},
	"updated_at": {Expr: "tm.updated_at", Kind: filter.Time},
}

// Tiebreak completes every team ordering, and it is the primary key so the order is
// total - cursor pagination needs that (see ticket.Tiebreak).
const Tiebreak = "tm.id"

// DefaultSort is name, ascending: what a picker wants. UNIQUE (workspace_id, name)
// from 0005 indexes exactly this order within the workspace the list is scoped to.
var DefaultSort = []filter.Order{{Field: "name"}}

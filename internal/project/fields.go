/*
╔═ fields.go ═══════════════════════════════════════════════════════════════════════════
║  project · what a caller may filter and sort on
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Fields         filter.Schema
║      Tiebreak       const
║      DefaultSort    var
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  the project list body's filter and sort
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package project

import "github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

// Fields is the project filter vocabulary, declared beside the service for the reason
// ticket.Fields gives (conventions §3). Every name is persisted public surface; adding
// one is free, renaming one is a data migration.
//
// team is the field a project picker needs: "the projects of the team this ticket is
// being filed under". It filters on the column, so it is the project_team_id_idx lookup
// it looks like.
var Fields = filter.Schema{
	"name":       {Expr: "p.name", Kind: filter.Text},
	"team":       {Expr: "p.team_id", Kind: filter.UUID},
	"created_at": {Expr: "p.created_at", Kind: filter.Time},
	"updated_at": {Expr: "p.updated_at", Kind: filter.Time},
}

// Tiebreak completes every project ordering. Names are not unique within a team (0006
// declines it), so without a unique last key two projects called "Core" have no fixed
// order and a cursor between them skips one.
const Tiebreak = "p.id"

// DefaultSort is name, ascending: what a picker wants.
var DefaultSort = []filter.Order{{Field: "name"}}

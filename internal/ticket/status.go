/*
╔═ status.go ═══════════════════════════════════════════════════════════════════════════
║  ticket · the writes and counts a status delete needs
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CountWithStatus    func
║      CountInTeam        func
║      MoveStatus         func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/team  →  DeleteStatus, inside its transaction
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Tx is whatever can run statements inside the caller's transaction. These three are
// package functions rather than Service methods because the caller's transaction is
// the point: deleting a status and re-pointing its tickets commit together or not at
// all, and a Service method would open its own.
//
// The ticket table keeps one writer. internal/team asks this package to do the moving
// instead of writing ticket itself, which sqlguard would refuse.
type Tx interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CountWithStatus counts every ticket pointing at the status, archived ones included:
// an archived ticket is restorable, and a restored one with no status is the same
// problem as a live one.
//
// Unscoped. The caller has already proved it can reach the status's team.
func CountWithStatus(ctx context.Context, q Tx, statusID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM ticket t WHERE t.status_id = $1::uuid`, statusID).Scan(&n); err != nil {
		return 0, fmt.Errorf("ticket: count with status: %w", err)
	}

	return n, nil
}

// CountInTeam counts every ticket in the team's projects, archived included. It is the
// "do tickets exist" test for deleting a team's last status.
func CountInTeam(ctx context.Context, q Tx, teamID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM ticket t JOIN project p ON p.id = t.project_id
		  WHERE p.team_id = $1::uuid`, teamID).Scan(&n); err != nil {
		return 0, fmt.Errorf("ticket: count in team: %w", err)
	}

	return n, nil
}

// MoveStatus re-points every ticket at from to to, as a normal write: version + 1,
// updated_at and updated_by set, so a client holding the old copy sees it as stale and
// an If-Match on it conflicts. Returns the tickets as written, for the change feed.
//
// Archived tickets move too. The search index does not carry status, so nothing is
// reindexed.
func MoveStatus(ctx context.Context, q Tx, accountID, from, to string) ([]Ticket, error) {
	rows, err := q.Query(ctx,
		`UPDATE ticket t
		    SET status_id = $2::uuid, updated_at = now(),
		        updated_by = $3::uuid, version = t.version + 1
		  WHERE t.status_id = $1::uuid
		 RETURNING `+columns, from, to, accountID)
	if err != nil {
		return nil, fmt.Errorf("ticket: move status: %w", err)
	}
	defer rows.Close()

	var out []Ticket
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(t.dest()...); err != nil {
			return nil, fmt.Errorf("ticket: move status scan: %w", err)
		}
		out = append(out, t)
	}

	return out, rows.Err()
}

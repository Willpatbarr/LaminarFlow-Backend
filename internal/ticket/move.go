/*
╔═ move.go ═════════════════════════════════════════════════════════════════════════════
║  ticket · dropping a card into a board column
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      MoveParams            struct
║      ErrNeighbourNotFound  error
║      Service.Move          method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/tickets/{id}/move
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNeighbourNotFound means before_id or after_id names no live ticket in the target
// column: absent, archived, in another project or status, or the ticket being moved.
var ErrNeighbourNotFound = errors.New("ticket: no such ticket in the target column")

// positionStep is the gap left between neighbours, so a drop between two cards takes
// the midpoint and writes one row. 0034 backfills with the same number.
const positionStep = 1024

/*
┏━ MoveParams ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one drop
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID          string
┃      StatusID    string      the column, as its status
┃      Position    *int        written as given
┃      BeforeID    *string     land just above this ticket
┃      AfterID     *string     land just below this ticket
┃      IfVersion   *int        nil = no precondition
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api
*/

// At most one of Position, BeforeID and AfterID is set - the API refuses more - and none
// means the end of the column.
type MoveParams struct {
	ID        string
	StatusID  string
	Position  *int
	BeforeID  *string
	AfterID   *string
	IfVersion *int
}

/*
┌─ ticket ────────────────────────────────────────
│  moves a ticket to a status and a place in it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            MoveParams
├─ out ───────────────────────────────────────────
│      Ticket                     as written, Version + 1
│      error                      ErrNotFound · *Conflict · ErrStatusNotFound
│                                 ErrNeighbourNotFound
├─ example ───────────────────────────────────────
│      after_id = B, B at 2048, C at 3072  →  Position 2560
*/

// Move is one write per drop: the ticket's status and position change together, and the
// ticket is the only row that gets a new version. It is written like any edit - version
// + 1, updated_at and updated_by - so a client holding the old copy conflicts on
// If-Match and the change feed announces it as ticket.updated.
//
// Where a card lands is computed from its neighbours inside the transaction that writes
// it, with the ticket's row locked, so two drops of one card serialise. When there is no
// whole number between the neighbours the target column is renumbered first, 1024 apart,
// in one statement. That rewrite changes position on tickets that are not the one being
// moved and does not bump their version or announce them: position is a hint, and a
// client that sees an order it does not expect re-lists.
func (s *Service) Move(ctx context.Context, accountID string, p MoveParams) (Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Scope, liveness and the version precondition are all predicates of the one
	// statement that takes the lock, so a refusal is explained by the same noRow every
	// other write uses.
	var projectID, teamID string
	err = tx.QueryRow(ctx,
		expand(`SELECT t.project_id::text,
		               (SELECT pr.team_id::text FROM project pr WHERE pr.id = t.project_id)
		          FROM ticket t
		         WHERE t.id = $1::uuid AND `+liveOnly+` AND `+callerCanReach+`
		           AND ($3::int IS NULL OR t.version = $3::int)
		           FOR UPDATE OF t`, "$2"),
		p.ID, accountID, p.IfVersion,
	).Scan(&projectID, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, s.noRow(ctx, tx, accountID, p.ID, p.IfVersion, false)
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: move lock: %w", err)
	}

	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM status WHERE id = $1::uuid AND team_id = $2::uuid)`,
		p.StatusID, teamID).Scan(&ok); err != nil {
		return Ticket{}, fmt.Errorf("ticket: move status: %w", err)
	}
	if !ok {
		return Ticket{}, ErrStatusNotFound
	}

	pos, err := s.place(ctx, tx, projectID, p)
	if err != nil {
		return Ticket{}, err
	}

	var t Ticket
	err = tx.QueryRow(ctx,
		`UPDATE ticket t
		    SET status_id = $2::uuid, position = $3, updated_at = now(),
		        updated_by = $4::uuid, version = t.version + 1
		  WHERE t.id = $1::uuid
		 RETURNING `+columns,
		p.ID, p.StatusID, pos, accountID,
	).Scan(t.dest()...)
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: move: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, nil
}

// place picks the position a moved ticket is written with.
func (s *Service) place(ctx context.Context, tx pgx.Tx, projectID string, p MoveParams) (int, error) {
	switch {
	case p.Position != nil:
		return *p.Position, nil

	case p.BeforeID != nil:
		return s.between(ctx, tx, projectID, p, *p.BeforeID, true)

	case p.AfterID != nil:
		return s.between(ctx, tx, projectID, p, *p.AfterID, false)
	}

	var end int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(position), 0) FROM ticket
		  WHERE project_id = $1::uuid AND status_id = $2::uuid AND archived_at IS NULL
		    AND id <> $3::uuid`,
		projectID, p.StatusID, p.ID).Scan(&end); err != nil {
		return 0, fmt.Errorf("ticket: move end: %w", err)
	}

	return end + positionStep, nil
}

// between finds a number on the near side of the neighbour, strictly between it and the
// card on its other side. At most one renumber is needed: after it the neighbours are
// 1024 apart.
func (s *Service) between(ctx context.Context, tx pgx.Tx, projectID string, p MoveParams, neighbour string, before bool) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var (
			nPos     int
			gap      *int
			hasOther bool
		)

		// The comparison and the order are the column's own, (position, id), so a tie on
		// position is read the way every list reads it.
		cmp, order := "<", "DESC"
		if !before {
			cmp, order = ">", "ASC"
		}

		err := tx.QueryRow(ctx,
			`SELECT n.position,
			        o.position,
			        o.position IS NOT NULL
			   FROM ticket n
			   LEFT JOIN LATERAL (
			        SELECT x.position FROM ticket x
			         WHERE x.project_id = n.project_id AND x.status_id = n.status_id
			           AND x.archived_at IS NULL AND x.id <> $3::uuid
			           AND (x.position, x.id) `+cmp+` (n.position, n.id)
			         ORDER BY x.position `+order+`, x.id `+order+` LIMIT 1) o ON true
			  WHERE n.id = $1::uuid AND n.project_id = $2::uuid AND n.status_id = $4::uuid
			    AND n.archived_at IS NULL AND n.id <> $3::uuid`,
			neighbour, projectID, p.ID, p.StatusID,
		).Scan(&nPos, &gap, &hasOther)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNeighbourNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("ticket: move neighbour: %w", err)
		}

		if !hasOther {
			if before {
				return nPos - positionStep, nil
			}

			return nPos + positionStep, nil
		}

		lo, hi := *gap, nPos
		if !before {
			lo, hi = nPos, *gap
		}
		if hi-lo >= 2 {
			return lo + (hi-lo)/2, nil
		}

		if attempt == 0 {
			if err := s.renumber(ctx, tx, projectID, p); err != nil {
				return 0, err
			}
		}
	}

	return 0, fmt.Errorf("ticket: move: no room between neighbours after renumbering")
}

// renumber spaces the target column's tickets 1024 apart in their current order,
// leaving out the ticket being moved.
func (s *Service) renumber(ctx context.Context, tx pgx.Tx, projectID string, p MoveParams) error {
	if _, err := tx.Exec(ctx,
		`UPDATE ticket t SET position = o.ord * `+fmt.Sprint(positionStep)+`
		   FROM (SELECT x.id, row_number() OVER (ORDER BY x.position, x.id) AS ord
		           FROM ticket x
		          WHERE x.project_id = $1::uuid AND x.status_id = $2::uuid
		            AND x.archived_at IS NULL AND x.id <> $3::uuid) o
		  WHERE t.id = o.id`,
		projectID, p.StatusID, p.ID); err != nil {
		return fmt.Errorf("ticket: renumber: %w", err)
	}

	return nil
}

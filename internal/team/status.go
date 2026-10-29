/*
╔═ status.go ═══════════════════════════════════════════════════════════════════════════
║  team · editing a team's statuses
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      StatusParams         struct
║      StatusPatch          struct
║      StatusInUse          struct
║      DeleteResult         struct
║      ErrStatusNotFound    error
║      ErrMoveToNotFound    error
║      ErrStatusSetWrong    error
║      Service.CreateStatus · UpdateStatus · DeleteStatus · ReorderStatuses
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/teams/{id}/statuses
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/jackc/pgx/v5"
)

// LAM-83. "No status is sacred" (Master Spec 3.4): a team may rename, add, reorder and
// delete any of them, the seeded three included, down to none.
//
// The code lives here rather than in a new internal/status package. status is read by
// ticket and project and written at team creation, so a separate package would have to
// be imported by team for the seeding it already does, and own a table whose bootstrap
// is not its own. team.go said the first ticket to give status a CRUD surface should
// claim it; this is that ticket, and boundary_test.go now guards status writes too.

// MaxStatusNameLength and MaxStatusColorLength are held at the API and in the service,
// not by a CHECK, for the reason MaxNameLength gives.
const (
	MaxStatusNameLength  = 100
	MaxStatusColorLength = 32
)

var (
	// ErrStatusNotFound is a status that does not exist, or belongs to another team.
	ErrStatusNotFound = errors.New("team: no such status")

	// ErrMoveToNotFound is a move_to that is not a status of this team, or is the status
	// being deleted.
	ErrMoveToNotFound = errors.New("team: no such status to move tickets to")

	// ErrStatusSetWrong is a reorder list that is partial, duplicated or foreign.
	ErrStatusSetWrong = errors.New("team: the order must name every status of this team exactly once")
)

// errCategory is the Reason for a category outside the closed set. It rides in a
// textlimit.FieldError so the API places it at body.category like any other field.
var errCategory = errors.New("must be one of not_started, in_progress, done")

// StatusInUse refuses a delete that would strand tickets.
type StatusInUse struct {
	// Count is the tickets that use the status, or, when Last, every ticket in the team.
	Count int
	// Last: the status is the team's only one, so no move_to could exist.
	Last bool
}

func (e *StatusInUse) Error() string {
	if e.Last {
		return fmt.Sprintf("team: last status with %d tickets", e.Count)
	}

	return fmt.Sprintf("team: status used by %d tickets", e.Count)
}

// StatusParams is a new status. Position nil means last.
type StatusParams struct {
	Name     string
	Color    string
	Category string
	Position *int
}

// StatusPatch is a rename and/or recolour and/or recategorise. Nil leaves a field alone.
type StatusPatch struct {
	Name     *string
	Color    *string
	Category *string
}

// DeleteResult is what a delete did: the status as it was, and the tickets it moved,
// as written, for the change feed.
type DeleteResult struct {
	Status      Status
	WorkspaceID string
	Moved       []ticket.Ticket

	// Unmapped is every board column that showed the status, for board.column.updated.
	Unmapped []ColumnRef
}

// ColumnRef names a board column without importing the board package.
type ColumnRef struct {
	BoardID  string
	ColumnID string
}

func validCategory(c string) bool {
	return c == "not_started" || c == "in_progress" || c == "done"
}

func checkStatusFields(name, color, category *string) error {
	if name != nil {
		if err := textlimit.Required("name", *name, MaxStatusNameLength); err != nil {
			return err
		}
	}
	if color != nil {
		if err := textlimit.Required("color", *color, MaxStatusColorLength); err != nil {
			return err
		}
	}
	if category != nil && !validCategory(*category) {
		return &textlimit.FieldError{Field: "category", Reason: errCategory}
	}

	return nil
}

// reach proves the caller is a member of the team's workspace, and returns the
// workspace. Every method here starts with it, on its own, so that a caller outside
// learns nothing from any later check.
func reach(ctx context.Context, tx pgx.Tx, accountID, teamID string) (string, error) {
	var ws string
	err := tx.QueryRow(ctx,
		`SELECT tm.workspace_id::text FROM team tm
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE tm.id = $1::uuid`, teamID, accountID).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("team: reach: %w", err)
	}

	return ws, nil
}

const statusColumns = `id::text, name, color, position, category`

func scanStatus(row pgx.Row) (Status, error) {
	var st Status
	err := row.Scan(&st.ID, &st.Name, &st.Color, &st.Position, &st.Category)

	return st, err
}

// CreateStatus adds a status. With no position it goes last: one past the highest, which
// is 1 for a team with none. Positions are hints and duplicates are legal (0010), so an
// explicit position is stored as given.
func (s *Service) CreateStatus(ctx context.Context, accountID, teamID string, p StatusParams) (Status, string, error) {
	if err := checkStatusFields(&p.Name, &p.Color, &p.Category); err != nil {
		return Status{}, "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Status{}, "", fmt.Errorf("team: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	ws, err := reach(ctx, tx, accountID, teamID)
	if err != nil {
		return Status{}, "", err
	}

	st, err := scanStatus(tx.QueryRow(ctx,
		`INSERT INTO status (team_id, name, color, position, category)
		 VALUES ($1::uuid, $2, $3,
		         COALESCE($4::int, (SELECT COALESCE(MAX(position), 0) + 1 FROM status WHERE team_id = $1::uuid)),
		         $5)
		 RETURNING `+statusColumns,
		teamID, p.Name, p.Color, p.Position, p.Category))
	if err != nil {
		return Status{}, "", fmt.Errorf("team: create status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Status{}, "", fmt.Errorf("team: commit: %w", err)
	}

	return st, ws, nil
}

// UpdateStatus changes the fields given. Returns the workspace for the change feed.
func (s *Service) UpdateStatus(ctx context.Context, accountID, teamID, statusID string, p StatusPatch) (Status, string, error) {
	if err := checkStatusFields(p.Name, p.Color, p.Category); err != nil {
		return Status{}, "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Status{}, "", fmt.Errorf("team: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	ws, err := reach(ctx, tx, accountID, teamID)
	if err != nil {
		return Status{}, "", err
	}

	st, err := scanStatus(tx.QueryRow(ctx,
		`UPDATE status
		    SET name = COALESCE($3, name), color = COALESCE($4, color),
		        category = COALESCE($5, category), updated_at = now()
		  WHERE id = $1::uuid AND team_id = $2::uuid
		 RETURNING `+statusColumns,
		statusID, teamID, p.Name, p.Color, p.Category))
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, "", ErrStatusNotFound
	}
	if err != nil {
		return Status{}, "", fmt.Errorf("team: update status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Status{}, "", fmt.Errorf("team: commit: %w", err)
	}

	return st, ws, nil
}

// DeleteStatus deletes a status, first moving its tickets to moveTo when there is one.
//
// ticket.status_id is ON DELETE SET NULL (0011), so the database would happily leave
// tickets statusless; the StatusInUse refusal here is the only guard, and the status row
// is locked for the transaction so a ticket cannot be pointed at it between the count
// and the delete.
//
// The deleted status is unmapped from every board column that showed it, whether or not
// tickets move (LAM-134). The mapping is not re-pointed at moveTo: that could put moveTo
// in two columns of one board. The affected columns are returned for the change feed.
func (s *Service) DeleteStatus(ctx context.Context, accountID, teamID, statusID string, moveTo *string) (DeleteResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("team: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	ws, err := reach(ctx, tx, accountID, teamID)
	if err != nil {
		return DeleteResult{}, err
	}

	st, err := scanStatus(tx.QueryRow(ctx,
		`SELECT `+statusColumns+` FROM status
		  WHERE id = $1::uuid AND team_id = $2::uuid FOR UPDATE`, statusID, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteResult{}, ErrStatusNotFound
	}
	if err != nil {
		return DeleteResult{}, fmt.Errorf("team: lock status: %w", err)
	}

	if moveTo != nil {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM status WHERE id = $1::uuid AND team_id = $2::uuid AND id <> $3::uuid)`,
			*moveTo, teamID, statusID).Scan(&ok); err != nil {
			return DeleteResult{}, fmt.Errorf("team: check move_to: %w", err)
		}
		if !ok {
			return DeleteResult{}, ErrMoveToNotFound
		}
		// Shared lock: the target cannot be deleted by a concurrent request while
		// this one is pointing tickets at it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM status WHERE id = $1::uuid FOR SHARE`, *moveTo); err != nil {
			return DeleteResult{}, fmt.Errorf("team: lock move_to: %w", err)
		}
	}

	used, err := ticket.CountWithStatus(ctx, tx, statusID)
	if err != nil {
		return DeleteResult{}, err
	}

	if moveTo == nil {
		var statuses int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM status WHERE team_id = $1::uuid`, teamID).Scan(&statuses); err != nil {
			return DeleteResult{}, fmt.Errorf("team: count statuses: %w", err)
		}

		if statuses == 1 {
			// The last one: no other status can be move_to, so the only way through
			// is a team with no tickets at all.
			n, err := ticket.CountInTeam(ctx, tx, teamID)
			if err != nil {
				return DeleteResult{}, err
			}
			if n > 0 {
				return DeleteResult{}, &StatusInUse{Count: n, Last: true}
			}
		} else if used > 0 {
			return DeleteResult{}, &StatusInUse{Count: used}
		}
	}

	var moved []ticket.Ticket
	if moveTo != nil {
		if moved, err = ticket.MoveStatus(ctx, tx, accountID, statusID, *moveTo); err != nil {
			return DeleteResult{}, err
		}
	}

	// The status leaves every board column that showed it; moveTo is never mapped on its
	// behalf (LAM-134). moveTo may already sit in another column of the same board, and a
	// status sits in one column of a board, so re-pointing the mapping put it in two. A
	// column left with no status is allowed and stays on its board, empty.
	rows, err := tx.Query(ctx,
		`DELETE FROM board_column_status bcs
		  USING board_column c
		  WHERE bcs.status_id = $1::uuid AND c.id = bcs.board_column_id
		RETURNING c.board_id::text, c.id::text`, statusID)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("team: unmap board columns: %w", err)
	}
	var unmapped []ColumnRef
	for rows.Next() {
		var ref ColumnRef
		if err := rows.Scan(&ref.BoardID, &ref.ColumnID); err != nil {
			rows.Close()
			return DeleteResult{}, fmt.Errorf("team: unmap scan: %w", err)
		}
		unmapped = append(unmapped, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DeleteResult{}, fmt.Errorf("team: unmap rows: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM status WHERE id = $1::uuid`, statusID); err != nil {
		return DeleteResult{}, fmt.Errorf("team: delete status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DeleteResult{}, fmt.Errorf("team: commit: %w", err)
	}

	return DeleteResult{Status: st, WorkspaceID: ws, Moved: moved, Unmapped: unmapped}, nil
}

// ReorderStatuses takes the complete ordered list, for the reasons aspect.Reorder gives,
// and writes positions 1..n in one statement. Returns the statuses whose position
// changed, as written, for the change feed, and the workspace.
func (s *Service) ReorderStatuses(ctx context.Context, accountID, teamID string, ids []string) ([]Status, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("team: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	ws, err := reach(ctx, tx, accountID, teamID)
	if err != nil {
		return nil, "", err
	}

	current, err := Statuses(ctx, tx, teamID)
	if err != nil {
		return nil, "", err
	}
	if !sameStatusSet(current, ids) {
		return nil, "", ErrStatusSetWrong
	}

	rows, err := tx.Query(ctx,
		`UPDATE status s
		    SET position = o.ord, updated_at = now()
		   FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, ord)
		  WHERE s.id = o.id AND s.team_id = $1::uuid AND s.position IS DISTINCT FROM o.ord
		 RETURNING s.`+statusColumns, teamID, ids)
	if err != nil {
		return nil, "", fmt.Errorf("team: reorder statuses: %w", err)
	}

	var changed []Status
	for rows.Next() {
		st, err := scanStatus(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("team: reorder scan: %w", err)
		}
		changed = append(changed, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("team: reorder rows: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("team: commit: %w", err)
	}

	return changed, ws, nil
}

// sameStatusSet is true when ids names every current status exactly once.
func sameStatusSet(current []Status, ids []string) bool {
	if len(current) != len(ids) {
		return false
	}

	want := make(map[string]bool, len(current))
	for _, st := range current {
		want[st.ID] = true
	}
	for _, id := range ids {
		if !want[id] {
			return false
		}
		delete(want, id)
	}

	return len(want) == 0
}

/*
╔═ epic.go ═════════════════════════════════════════════════════════════════════════════
║  epic · project-scoped CRUD
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service            struct
║      Epic               struct
║      ErrNotFound        error
║      ErrProjectNotFound error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/epics
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package epic owns the epic table (0022): a named group of tickets inside one project.
//
// Deleting an epic does not touch its tickets' rows from here - the foreign key sets
// ticket.epic_id to null (0022, SET NULL, not CASCADE) - so a ticket's version does not
// move when its epic goes. The change feed's epic.deleted is how a client learns.
package epic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound collapses absent and out-of-scope, as every service here does.
	ErrNotFound = errors.New("epic: not found")

	// ErrProjectNotFound is a project that is absent or out of scope.
	ErrProjectNotFound = errors.New("epic: no such project")
)

// The longest an epic's text may be, in characters. Held here and at the API, not by a
// CHECK, for the reason ticket.MaxTitleLength gives.
const (
	MaxNameLength        = 100
	MaxDescriptionLength = 50000
)

/*
┏━ Epic ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one epic as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      ProjectID      string
┃      WorkspaceID    string      for the change feed
┃      Name           string
┃      Description    string      never null, '' when unset
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Update · Delete
*/

type Epic struct {
	ID          string
	ProjectID   string
	WorkspaceID string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Patch is an update. A nil member is left alone.
type Patch struct {
	Name        *string
	Description *string
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  owner of the epic table
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool    *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · List · Get · Update · Delete
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// callerCanReach is the scoping predicate: the caller is a member of the workspace the
// epic's project's team is in. $ACCOUNT is filled in by expand.
const callerCanReach = `EXISTS (
	        SELECT 1 FROM project p
	          JOIN team tm ON tm.id = p.team_id
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE p.id = e.project_id)`

const columns = `e.id::text, e.project_id::text,
	(SELECT tm.workspace_id::text FROM project p JOIN team tm ON tm.id = p.team_id WHERE p.id = e.project_id),
	e.name, e.description, e.created_at, e.updated_at`

func (e *Epic) dest() []any {
	return []any{&e.ID, &e.ProjectID, &e.WorkspaceID, &e.Name, &e.Description, &e.CreatedAt, &e.UpdatedAt}
}

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

/*
┌─ epic ──────────────────────────────────────────
│  creates an epic in a project the caller can reach
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      projectID    string
│      name         string        required
│      description  string        may be empty
├─ out ───────────────────────────────────────────
│      Epic
│      error        ErrProjectNotFound · *textlimit.FieldError
*/

func (s *Service) Create(ctx context.Context, accountID, projectID, name, description string) (Epic, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Epic{}, err
	}
	if err := textlimit.Optional("description", description, MaxDescriptionLength); err != nil {
		return Epic{}, err
	}

	var e Epic
	err := s.pool.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO epic (project_id, name, description)
		     SELECT p.id, $2, $3
		       FROM project p
		       JOIN team tm ON tm.id = p.team_id
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $4::uuid
		      WHERE p.id = $1::uuid
		     RETURNING *)
		 SELECT `+columns+` FROM ins e`,
		projectID, name, description, accountID,
	).Scan(e.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epic{}, ErrProjectNotFound
	}
	if err != nil {
		return Epic{}, fmt.Errorf("epic: insert: %w", err)
	}

	return e, nil
}

/*
┌─ epic ──────────────────────────────────────────
│  every epic of a project, oldest first
├─ out ───────────────────────────────────────────
│      []Epic
│      error        ErrProjectNotFound
*/

// List is one page, always: a project has a handful of epics, so there is no vocabulary
// to filter or sort and never a second page (the board list makes the same call).
func (s *Service) List(ctx context.Context, accountID, projectID string) ([]Epic, error) {
	var reachable string
	err := s.pool.QueryRow(ctx,
		`SELECT p.id::text FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE p.id = $1::uuid`, projectID, accountID).Scan(&reachable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("epic: list scope: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM epic e WHERE e.project_id = $1::uuid ORDER BY e.created_at, e.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("epic: list: %w", err)
	}
	defer rows.Close()

	var out []Epic
	for rows.Next() {
		var e Epic
		if err := rows.Scan(e.dest()...); err != nil {
			return nil, fmt.Errorf("epic: list scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("epic: list rows: %w", err)
	}

	return out, nil
}

// Get returns one epic the caller may see.
func (s *Service) Get(ctx context.Context, accountID, id string) (Epic, error) {
	var e Epic
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM epic e WHERE e.id = $1::uuid AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(e.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epic{}, ErrNotFound
	}
	if err != nil {
		return Epic{}, fmt.Errorf("epic: get: %w", err)
	}

	return e, nil
}

/*
┌─ epic ──────────────────────────────────────────
│  changes an epic's name and/or description
├─ in ────────────────────────────────────────────
│      p    Patch       nil member = left alone
├─ out ───────────────────────────────────────────
│      Epic             as written
│      error            ErrNotFound · *textlimit.FieldError
*/

func (s *Service) Update(ctx context.Context, accountID, id string, p Patch) (Epic, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxNameLength); err != nil {
			return Epic{}, err
		}
	}
	if p.Description != nil {
		if err := textlimit.Optional("description", *p.Description, MaxDescriptionLength); err != nil {
			return Epic{}, err
		}
	}

	var e Epic
	err := s.pool.QueryRow(ctx,
		expand(`WITH upd AS (
		     UPDATE epic e
		        SET name = coalesce($2::text, e.name),
		            description = coalesce($3::text, e.description),
		            updated_at = now()
		      WHERE e.id = $1::uuid AND `+callerCanReach+`
		     RETURNING e.*)
		 SELECT `+columns+` FROM upd e`, "$4"),
		id, p.Name, p.Description, accountID,
	).Scan(e.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epic{}, ErrNotFound
	}
	if err != nil {
		return Epic{}, fmt.Errorf("epic: update: %w", err)
	}

	return e, nil
}

// Delete removes an epic and returns it as it was, for the change feed. Its tickets stay,
// with epic_id set to null by the foreign key (0022).
func (s *Service) Delete(ctx context.Context, accountID, id string) (Epic, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Epic{}, fmt.Errorf("epic: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var e Epic
	err = tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM epic e WHERE e.id = $1::uuid AND `+callerCanReach+` FOR UPDATE OF e`, "$2"),
		id, accountID,
	).Scan(e.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epic{}, ErrNotFound
	}
	if err != nil {
		return Epic{}, fmt.Errorf("epic: delete lookup: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM epic WHERE id = $1::uuid`, id); err != nil {
		return Epic{}, fmt.Errorf("epic: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Epic{}, fmt.Errorf("epic: commit: %w", err)
	}

	return e, nil
}

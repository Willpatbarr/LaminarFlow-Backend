/*
╔═ sprint.go ═══════════════════════════════════════════════════════════════════════════
║  sprint · project-scoped CRUD
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service              struct
║      Sprint               struct
║      Patch                struct
║      Nullable             struct
║      ErrNotFound          error
║      ErrProjectNotFound   error
║      ErrEndBeforeStart    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/sprints
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package sprint owns the sprint table (0012): a named, optionally dated window of work
// inside one project.
//
// Which tickets are in a sprint is ticket_sprint (0013), and that is written with the
// ticket - joining a sprint is a change to the ticket, so it moves its version and is
// announced as ticket.updated. See internal/ticket's AddToSprint and RemoveFromSprint.
// Deleting a sprint cascades its memberships away without touching the tickets' rows.
package sprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound collapses absent and out-of-scope, as every service here does.
	ErrNotFound = errors.New("sprint: not found")

	// ErrProjectNotFound is a project that is absent or out of scope.
	ErrProjectNotFound = errors.New("sprint: no such project")

	// ErrEndBeforeStart is end_date earlier than start_date: 0012's CHECK, by name.
	ErrEndBeforeStart = errors.New("sprint: end_date is before start_date")
)

// MaxNameLength bounds a sprint's name, in characters.
const MaxNameLength = 100

// endsAfterItStarts is 0012's CHECK constraint, which is how a violation is told apart
// from any other by name rather than by message.
const endsAfterItStarts = "sprint_ends_after_it_starts"

/*
┏━ Sprint ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one sprint as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      ProjectID      string
┃      WorkspaceID    string      for the change feed
┃      Name           string
┃      StartDate      *string     YYYY-MM-DD, null when undated
┃      EndDate        *string     YYYY-MM-DD, null when undated
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Update · Delete
*/

type Sprint struct {
	ID          string
	ProjectID   string
	WorkspaceID string
	Name        string
	StartDate   *string
	EndDate     *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Nullable is a value an update may leave alone (Set false), set, or clear (Set true,
// Value nil).
type Nullable struct {
	Set   bool
	Value *string
}

// Patch is an update. A nil or unset member is left alone.
type Patch struct {
	Name      *string
	StartDate Nullable
	EndDate   Nullable
}

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const callerCanReach = `EXISTS (
	        SELECT 1 FROM project p
	          JOIN team tm ON tm.id = p.team_id
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE p.id = sp.project_id)`

const columns = `sp.id::text, sp.project_id::text,
	(SELECT tm.workspace_id::text FROM project p JOIN team tm ON tm.id = p.team_id WHERE p.id = sp.project_id),
	sp.name, sp.start_date::text, sp.end_date::text, sp.created_at, sp.updated_at`

func (s *Sprint) dest() []any {
	return []any{&s.ID, &s.ProjectID, &s.WorkspaceID, &s.Name, &s.StartDate, &s.EndDate, &s.CreatedAt, &s.UpdatedAt}
}

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

// writeError maps the CHECK's violation; nil for anything else.
func writeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == endsAfterItStarts {
		return ErrEndBeforeStart
	}

	return nil
}

// Create inserts a sprint into a project the caller can reach. Dates are YYYY-MM-DD or
// nil; an end before the start is ErrEndBeforeStart.
func (s *Service) Create(ctx context.Context, accountID, projectID, name string, start, end *string) (Sprint, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Sprint{}, err
	}

	var sp Sprint
	err := s.pool.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO sprint (project_id, name, start_date, end_date)
		     SELECT p.id, $2, $3::date, $4::date
		       FROM project p
		       JOIN team tm ON tm.id = p.team_id
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $5::uuid
		      WHERE p.id = $1::uuid
		     RETURNING *)
		 SELECT `+columns+` FROM ins sp`,
		projectID, name, start, end, accountID,
	).Scan(sp.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sprint{}, ErrProjectNotFound
	}
	if e := writeError(err); e != nil {
		return Sprint{}, e
	}
	if err != nil {
		return Sprint{}, fmt.Errorf("sprint: insert: %w", err)
	}

	return sp, nil
}

// List is every sprint of a project, soonest start first, undated ones last. One page,
// always, for the reason epic.List gives.
func (s *Service) List(ctx context.Context, accountID, projectID string) ([]Sprint, error) {
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
		return nil, fmt.Errorf("sprint: list scope: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM sprint sp WHERE sp.project_id = $1::uuid
		  ORDER BY sp.start_date NULLS LAST, sp.created_at, sp.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("sprint: list: %w", err)
	}
	defer rows.Close()

	var out []Sprint
	for rows.Next() {
		var sp Sprint
		if err := rows.Scan(sp.dest()...); err != nil {
			return nil, fmt.Errorf("sprint: list scan: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sprint: list rows: %w", err)
	}

	return out, nil
}

// Get returns one sprint the caller may see.
func (s *Service) Get(ctx context.Context, accountID, id string) (Sprint, error) {
	var sp Sprint
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM sprint sp WHERE sp.id = $1::uuid AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(sp.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sprint{}, ErrNotFound
	}
	if err != nil {
		return Sprint{}, fmt.Errorf("sprint: get: %w", err)
	}

	return sp, nil
}

// Update applies a patch; a member it leaves alone keeps its value. The CHECK is
// evaluated against the result, so moving only one date is judged against the other.
func (s *Service) Update(ctx context.Context, accountID, id string, p Patch) (Sprint, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxNameLength); err != nil {
			return Sprint{}, err
		}
	}

	var sp Sprint
	err := s.pool.QueryRow(ctx,
		expand(`WITH upd AS (
		     UPDATE sprint sp
		        SET name = coalesce($2::text, sp.name),
		            start_date = CASE WHEN $3::boolean THEN $4::date ELSE sp.start_date END,
		            end_date = CASE WHEN $5::boolean THEN $6::date ELSE sp.end_date END,
		            updated_at = now()
		      WHERE sp.id = $1::uuid AND `+callerCanReach+`
		     RETURNING sp.*)
		 SELECT `+columns+` FROM upd sp`, "$7"),
		id, p.Name, p.StartDate.Set, p.StartDate.Value, p.EndDate.Set, p.EndDate.Value, accountID,
	).Scan(sp.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sprint{}, ErrNotFound
	}
	if e := writeError(err); e != nil {
		return Sprint{}, e
	}
	if err != nil {
		return Sprint{}, fmt.Errorf("sprint: update: %w", err)
	}

	return sp, nil
}

// Delete removes a sprint and returns it as it was, for the change feed. Its memberships
// cascade away (0013); the tickets stay.
func (s *Service) Delete(ctx context.Context, accountID, id string) (Sprint, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Sprint{}, fmt.Errorf("sprint: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var sp Sprint
	err = tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM sprint sp WHERE sp.id = $1::uuid AND `+callerCanReach+` FOR UPDATE OF sp`, "$2"),
		id, accountID,
	).Scan(sp.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sprint{}, ErrNotFound
	}
	if err != nil {
		return Sprint{}, fmt.Errorf("sprint: delete lookup: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM sprint WHERE id = $1::uuid`, id); err != nil {
		return Sprint{}, fmt.Errorf("sprint: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Sprint{}, fmt.Errorf("sprint: commit: %w", err)
	}

	return sp, nil
}

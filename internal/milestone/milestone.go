/*
╔═ milestone.go ════════════════════════════════════════════════════════════════════════
║  milestone · project-scoped CRUD
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service              struct
║      Milestone           struct
║      Patch               struct
║      Nullable            struct
║      ErrNotFound         error
║      ErrProjectNotFound  error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/milestones
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package milestone owns the milestone table (0033): a named goal of a project, with an
// optional target date.
//
// LAM-82 decided there is no separate Release entity: a release is a milestone, and one
// can be added later. So target_date is nullable and nothing here knows the word.
//
// Deleting a milestone does not touch its tickets' rows from here - the foreign key sets
// ticket.milestone_id to null - so a ticket's version does not move when its milestone
// goes. The change feed's milestone.deleted is how a client learns.
package milestone

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
	ErrNotFound = errors.New("milestone: not found")

	// ErrProjectNotFound is a project that is absent or out of scope.
	ErrProjectNotFound = errors.New("milestone: no such project")
)

// The longest a milestone's text may be, in characters.
const (
	MaxNameLength        = 100
	MaxDescriptionLength = 50000
)

/*
┏━ Milestone ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one milestone as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      ProjectID      string
┃      WorkspaceID    string      for the change feed
┃      Name           string
┃      Description    string      never null, '' when unset
┃      TargetDate     *string     YYYY-MM-DD, null when undated
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Update · Delete
*/

type Milestone struct {
	ID          string
	ProjectID   string
	WorkspaceID string
	Name        string
	Description string
	TargetDate  *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Nullable is a value an update may leave alone (Set false), set, or clear (Set true,
// Value nil) - the three states a nullable column needs and a bare pointer cannot give.
type Nullable struct {
	Set   bool
	Value *string
}

// Patch is an update. A nil or unset member is left alone.
type Patch struct {
	Name        *string
	Description *string
	TargetDate  Nullable
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
	         WHERE p.id = ms.project_id)`

const columns = `ms.id::text, ms.project_id::text,
	(SELECT tm.workspace_id::text FROM project p JOIN team tm ON tm.id = p.team_id WHERE p.id = ms.project_id),
	ms.name, ms.description, ms.target_date::text, ms.created_at, ms.updated_at`

func (m *Milestone) dest() []any {
	return []any{&m.ID, &m.ProjectID, &m.WorkspaceID, &m.Name, &m.Description, &m.TargetDate, &m.CreatedAt, &m.UpdatedAt}
}

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

// Create inserts a milestone into a project the caller can reach. targetDate is
// YYYY-MM-DD or nil.
func (s *Service) Create(ctx context.Context, accountID, projectID, name, description string, targetDate *string) (Milestone, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Milestone{}, err
	}
	if err := textlimit.Optional("description", description, MaxDescriptionLength); err != nil {
		return Milestone{}, err
	}

	var m Milestone
	err := s.pool.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO milestone (project_id, name, description, target_date)
		     SELECT p.id, $2, $3, $4::date
		       FROM project p
		       JOIN team tm ON tm.id = p.team_id
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $5::uuid
		      WHERE p.id = $1::uuid
		     RETURNING *)
		 SELECT `+columns+` FROM ins ms`,
		projectID, name, description, targetDate, accountID,
	).Scan(m.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Milestone{}, ErrProjectNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("milestone: insert: %w", err)
	}

	return m, nil
}

// List is every milestone of a project: dated ones soonest first, then undated ones,
// oldest first. One page, always, for the reason epic.List gives.
func (s *Service) List(ctx context.Context, accountID, projectID string) ([]Milestone, error) {
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
		return nil, fmt.Errorf("milestone: list scope: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM milestone ms WHERE ms.project_id = $1::uuid
		  ORDER BY ms.target_date NULLS LAST, ms.created_at, ms.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("milestone: list: %w", err)
	}
	defer rows.Close()

	var out []Milestone
	for rows.Next() {
		var m Milestone
		if err := rows.Scan(m.dest()...); err != nil {
			return nil, fmt.Errorf("milestone: list scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("milestone: list rows: %w", err)
	}

	return out, nil
}

// Get returns one milestone the caller may see.
func (s *Service) Get(ctx context.Context, accountID, id string) (Milestone, error) {
	var m Milestone
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM milestone ms WHERE ms.id = $1::uuid AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(m.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Milestone{}, ErrNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("milestone: get: %w", err)
	}

	return m, nil
}

// Update applies a patch; a member it leaves alone keeps its value.
func (s *Service) Update(ctx context.Context, accountID, id string, p Patch) (Milestone, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxNameLength); err != nil {
			return Milestone{}, err
		}
	}
	if p.Description != nil {
		if err := textlimit.Optional("description", *p.Description, MaxDescriptionLength); err != nil {
			return Milestone{}, err
		}
	}

	var m Milestone
	err := s.pool.QueryRow(ctx,
		expand(`WITH upd AS (
		     UPDATE milestone ms
		        SET name = coalesce($2::text, ms.name),
		            description = coalesce($3::text, ms.description),
		            target_date = CASE WHEN $4::boolean THEN $5::date ELSE ms.target_date END,
		            updated_at = now()
		      WHERE ms.id = $1::uuid AND `+callerCanReach+`
		     RETURNING ms.*)
		 SELECT `+columns+` FROM upd ms`, "$6"),
		id, p.Name, p.Description, p.TargetDate.Set, p.TargetDate.Value, accountID,
	).Scan(m.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Milestone{}, ErrNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("milestone: update: %w", err)
	}

	return m, nil
}

// Delete removes a milestone and returns it as it was, for the change feed. Its tickets
// stay, with milestone_id set to null by the foreign key (0033).
func (s *Service) Delete(ctx context.Context, accountID, id string) (Milestone, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Milestone{}, fmt.Errorf("milestone: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var m Milestone
	err = tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM milestone ms WHERE ms.id = $1::uuid AND `+callerCanReach+` FOR UPDATE OF ms`, "$2"),
		id, accountID,
	).Scan(m.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Milestone{}, ErrNotFound
	}
	if err != nil {
		return Milestone{}, fmt.Errorf("milestone: delete lookup: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM milestone WHERE id = $1::uuid`, id); err != nil {
		return Milestone{}, fmt.Errorf("milestone: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Milestone{}, fmt.Errorf("milestone: commit: %w", err)
	}

	return m, nil
}

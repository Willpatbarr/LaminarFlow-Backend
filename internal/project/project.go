/*
╔═ project.go ══════════════════════════════════════════════════════════════════════════
║  project · creation, and the board a project cannot render without
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service         struct
║      Project         struct
║      Column          struct
║      CreateParams    struct
║      ErrNotFound     error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/projects
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package project creates projects, and seeds the board one starts with.
//
// LAM-43, second of two bootstrap points. The ticket was written as though seeding were
// one hook on team creation, and board is project-scoped - so the obligation is real
// and fires somewhere else. Splitting it here rather than widening team creation is
// what makes each hook seed only rows its own entity owns.
//
// The two are not independent. A board's columns map to statuses through
// board_column_status, which carries a composite team_id, so a seeded board can only
// map statuses belonging to its own project's team - which means the team bootstrap has
// to have run first. It has, by the time any project exists: internal/team seeds inside
// the transaction that creates the team.
package project

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers a team that is absent or outside the caller's reach.
var ErrNotFound = errors.New("project: not found")

// defaultBoardName is the board every project starts with. One board, not a set: 0025
// is explicit that nothing privileges one board over another and there is no default
// flag, so this is simply the first one, and a team that wants three makes two more.
const defaultBoardName = "Board"

// defaultGroupBy is board.group_by's only legal value today. 0025's CHECK holds exactly
// 'status', and LAM-52's board.column_sources registry offers exactly the same set -
// internal/setting's tests assert the two agree, so this constant cannot drift past
// either of them without something failing.
const defaultGroupBy = "status"

/*
┏━ Column ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one seeded board column, and the status it shows
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID          string
┃      Name        string
┃      Position    int
┃      StatusID    string
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create                 one per team status
*/

type Column struct {
	ID       string
	Name     string
	Position int
	StatusID string
}

/*
┏━ Project ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  a project, and the board its creation seeded
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID          string
┃      TeamID      string
┃      Name        string
┃      BoardID     string      empty when bare
┃      Columns     []Column    one per team status
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get
*/

type Project struct {
	ID        string
	TeamID    string
	Name      string
	BoardID   string
	Columns   []Column
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateParams is a new project's writable state plus the same bootstrap choice team
// creation offers, for the same importer.
type CreateParams struct {
	TeamID string
	Name   string
	Bare   bool
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  project creation, and its board bootstrap
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create(ctx, accountID, CreateParams)   →  Project, error
┃      Get(ctx, accountID, id)                →  Project, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

// Writes are guarded, reads are not - see boundary_test.go, and internal/team's for the
// argument. project is joined by every scoping predicate here and written by exactly
// one function.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service creating projects through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

/*
┌─ project ───────────────────────────────────────
│  creates a project and the board it renders with
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Project                   with its board
│      error                     ErrNotFound
├─ example ───────────────────────────────────────
│      "Core"  →  a board with 3 columns
*/

// Create is one transaction: the project, its board, the board's columns and their
// status mappings, or none of them.
//
// A project with no board is legal and must render - 0025 says so - so the board is a
// convenience rather than an invariant. It is seeded anyway because a project whose
// board view is empty on the first visit looks broken, and because the columns can only
// be built from the team's statuses, which is knowledge this is the last place to have.
func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (Project, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, fmt.Errorf("project: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var pr Project
	err = tx.QueryRow(ctx,
		`INSERT INTO project (team_id, name)
		 SELECT tm.id, $2
		   FROM team tm
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $3::uuid
		  WHERE tm.id = $1::uuid
		 RETURNING id::text, team_id::text, name, created_at, updated_at`,
		p.TeamID, p.Name, accountID,
	).Scan(&pr.ID, &pr.TeamID, &pr.Name, &pr.CreatedAt, &pr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("project: insert: %w", err)
	}

	if !p.Bare {
		if err := seedBoard(ctx, tx, &pr); err != nil {
			return Project{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("project: commit: %w", err)
	}

	return pr, nil
}

/*
┌─ project ───────────────────────────────────────
│  one project the caller may see
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      Project                   without its board
│      error                     ErrNotFound
*/

func (s *Service) Get(ctx context.Context, accountID, id string) (Project, error) {
	var pr Project

	err := s.pool.QueryRow(ctx,
		`SELECT p.id::text, p.team_id::text, p.name, p.created_at, p.updated_at
		   FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE p.id = $1::uuid`,
		id, accountID,
	).Scan(&pr.ID, &pr.TeamID, &pr.Name, &pr.CreatedAt, &pr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("project: get: %w", err)
	}

	return pr, nil
}

// seedBoard writes the board, one column per team status, and the mapping between them.
//
// The order is forced by the schema rather than chosen: board_column_status references
// board_column (id, team_id) and status (id, team_id), so a mapping written before its
// column is a foreign key violation. 0025's chain is what makes a column unable to hold
// another team's status, and it is also what makes this sequence the only one.
//
// A team created Bare has no statuses, so the board gets no columns. That is legal and
// is what "a project with no board is legal" already implies one level down.
func seedBoard(ctx context.Context, tx pgx.Tx, pr *Project) error {
	statuses, err := team.Statuses(ctx, tx, pr.TeamID)
	if err != nil {
		return err
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO board (project_id, team_id, name, group_by)
		 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
		pr.ID, pr.TeamID, defaultBoardName, defaultGroupBy,
	).Scan(&pr.BoardID)
	if err != nil {
		return fmt.Errorf("project: seed board: %w", err)
	}

	for _, st := range statuses {
		col := Column{Name: st.Name, Position: st.Position, StatusID: st.ID}

		// The column takes the status's name and position, so the first board a
		// team sees is the workflow it just configured rather than a second set
		// of names to reconcile with it. Renaming either afterwards does not move
		// the other - they are two rows, and 0025 keeps them that way on purpose.
		err = tx.QueryRow(ctx,
			`INSERT INTO board_column (board_id, team_id, name, position)
			 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
			pr.BoardID, pr.TeamID, col.Name, col.Position,
		).Scan(&col.ID)
		if err != nil {
			return fmt.Errorf("project: seed column %q: %w", st.Name, err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO board_column_status (board_column_id, status_id, team_id)
			 VALUES ($1::uuid, $2::uuid, $3::uuid)`,
			col.ID, col.StatusID, pr.TeamID); err != nil {
			return fmt.Errorf("project: map column %q: %w", st.Name, err)
		}

		pr.Columns = append(pr.Columns, col)
	}

	return nil
}

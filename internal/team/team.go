/*
╔═ team.go ═════════════════════════════════════════════════════════════════════════════
║  team · creation, and the rows a new team cannot be useful without
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service          struct
║      Team             struct
║      CreateParams     struct
║      DefaultStatuses  var
║      StarterTypes     var
║      ErrNotFound      error
║      ErrNameTaken     error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api      →  POST /api/v1/teams
║      internal/project  →  the statuses a seeded board maps to
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package team creates teams, and seeds what a team needs to be usable.
//
// LAM-43, first of two bootstrap points. The second is project creation, which seeds a
// board - see internal/project, and see the why block below for why they are two.
//
// Nothing here is a migration, and 0010 and 0014 both explain why at length: a
// migration runs once against a database, not once per team, so seeding in one gives
// the rows to whatever teams exist at the moment it is applied and silently nothing to
// every team created afterwards.
//
// Seeding runs inside the creation transaction. A team observable with no statuses is a
// team whose first ticket cannot be given one, and a half-configured team is a state
// nothing else in this codebase is written to expect.
package team

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers a workspace that is absent or outside the caller's reach.
var ErrNotFound = errors.New("team: not found")

// ErrNameTaken is 0005's UNIQUE (workspace_id, name), surfaced rather than leaked as a
// 23505 - two workspaces may each have a Platform team, one workspace may not.
var ErrNameTaken = errors.New("team: a team of that name already exists in this workspace")

/*
┏━ Status ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one seeded status, as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID          string
┃      Name        string
┃      Color       string
┃      Position    int
┃      Category    string    not_started · in_progress · done
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create                 three per bootstrapped team
*/

type Status struct {
	ID       string
	Name     string
	Color    string
	Position int
	Category string
}

// DefaultStatuses is LAM-17 step 3, and every value in it is a decision.
//
// Three, in one category each. category is the closed set reporting depends on, so a
// team that renames "To Do" to "Backlog" keeps a status that still counts as not
// started - which is the whole reason category exists separately from name.
//
// The colours are invented here because nothing has settled a palette and 0010 declines
// a CHECK for exactly that reason. They are ordinary column values a team can change,
// not a scheme anything reads.
//
// Positions are 1, 2, 3 and are ordering hints rather than slots: 0010 declines a
// unique index on (team_id, position), so a team inserting a status between two of
// these does not have to renumber anything.
var DefaultStatuses = []Status{
	{Name: "To Do", Color: "#94a3b8", Position: 1, Category: "not_started"},
	{Name: "In Progress", Color: "#3b82f6", Position: 2, Category: "in_progress"},
	{Name: "Done", Color: "#22c55e", Position: 3, Category: "done"},
}

// StarterTypes is LAM-21 step 3.
//
// Starter types, not system types. There is no is_system flag on aspect_type,
// deliberately, and LAM-21 says so: a team that deletes every one of these and writes
// its own five is using the product correctly.
var StarterTypes = []string{
	"Class", "Interface/Protocol", "Service", "Data Store", "Design Pattern",
}

// No default labels, which is LAM-49's open question answered.
//
// Statuses and aspect types are seeded because the product requires them to exist -
// a ticket needs somewhere to sit, and an aspect document needs a type. A label
// vocabulary is not like that: it is a team's own taxonomy, nothing breaks without one,
// and no ticket or spec names a set. Inventing one would be inventing structure nobody
// asked for, which is the rule this schema has followed throughout.
//
// label also carries UNIQUE (team_id, name) where status and aspect_type do not, so a
// wrong guess is actively expensive: a team wanting "Bug" has to delete the seeded
// "bug" first, and finds that out through a 23505.

// defaultViewName is the one saved view a team starts with. LAM-50's obligation, and
// the cheapest possible discharge of it: a shared list view with no filter, which is
// what "all the work" means.
const defaultViewName = "All tickets"

// defaultViewConfig is the filter and sort shape LAM-58 defined and LAM-59 consumes.
// Written out rather than built from those packages' types so that this stays a
// literal - saved_view.config is persisted, and a change to the shape is a data
// migration whether it comes from here or from a request body.
const defaultViewConfig = `{"filter":{"op":"and"},"sort":[{"field":"updated_at","desc":true}]}`

/*
┏━ Team ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  a team, and what its creation seeded
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      WorkspaceID    string
┃      Name           string
┃      Statuses       []Status       empty when bare
┃      AspectTypes    []string       empty when bare
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create                        one per call
*/

type Team struct {
	ID          string
	WorkspaceID string
	Name        string
	Statuses    []Status
	AspectTypes []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateParams is a new team's writable state plus the one bootstrap choice.
type CreateParams struct {
	WorkspaceID string
	Name        string

	// Bare skips seeding, which is LAM-21's "optional bootstrap step" made
	// explicit. It defaults to false because the default should be a usable team:
	// a caller who has to decide has no information with which to decide.
	//
	// The caller it exists for is an importer restoring a team that already has its
	// own statuses and aspect types, where seeding would mean deleting five rows
	// before writing five others - and where the seeded statuses would briefly be
	// the ones tickets got assigned to.
	Bare bool
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  team creation, and its bootstrap
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create(ctx, accountID, CreateParams)   →  Team, error
┃      Get(ctx, accountID, id)                →  Team, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

// Create is the only path that makes a team, and boundary_test.go is what enforces
// that rather than leaving it to convention. LAM-43 shipped without the rule; LAM-60
// added it once sqlguard could guard writes without a reader list naming most of the
// module.
//
// status, saved_view and the board tables still have no owner. A second writer to
// status produces a status rather than a broken team, so the invariant is not the same
// one - the first ticket that gives any of them a CRUD surface should claim it then.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service creating teams through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

/*
┌─ team ──────────────────────────────────────────
│  creates a team and everything it needs to work
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Team                      with what was seeded
│      error                     ErrNotFound · ErrNameTaken
├─ example ───────────────────────────────────────
│      "Platform"  →  3 statuses, 5 types, 1 view
*/

// Create is one transaction: the team, its statuses, its aspect types and its default
// view, or none of them.
//
// The seeding is not idempotent and does not try to be. status and aspect_type both
// allow duplicate names within a team - 0010 and 0014 decline UNIQUE (team_id, name) -
// so a bootstrap that ran twice would silently double every row rather than error.
// Running once is guaranteed here by construction: this is the only path that creates a
// team, and it seeds inside the same statement's transaction.
func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (Team, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Team{}, fmt.Errorf("team: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Gated on the caller being a member of the workspace, not on the workspace
	// existing. A workspace someone else is in returns no row, so this answers
	// ErrNotFound rather than creating a team the caller cannot then reach.
	var t Team
	err = tx.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name)
		 SELECT wm.workspace_id, $2
		   FROM workspace_member wm
		  WHERE wm.workspace_id = $1::uuid AND wm.account_id = $3::uuid
		 RETURNING id::text, workspace_id::text, name, created_at, updated_at`,
		p.WorkspaceID, p.Name, accountID,
	).Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Team{}, ErrNameTaken
	}
	if err != nil {
		return Team{}, fmt.Errorf("team: insert: %w", err)
	}

	if p.Bare {
		if err := tx.Commit(ctx); err != nil {
			return Team{}, fmt.Errorf("team: commit: %w", err)
		}
		return t, nil
	}

	if t.Statuses, err = seedStatuses(ctx, tx, t.ID); err != nil {
		return Team{}, err
	}

	// Through internal/aspect, because it owns aspect_type and sqlguard would fail
	// the build on an INSERT written here. That is the guard doing its job on the
	// first package that wanted to write around it.
	types, err := aspect.Bootstrap(ctx, tx, t.ID, StarterTypes)
	if err != nil {
		return Team{}, err
	}
	for _, at := range types {
		t.AspectTypes = append(t.AspectTypes, at.Name)
	}

	if err := seedDefaultView(ctx, tx, t.ID); err != nil {
		return Team{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Team{}, fmt.Errorf("team: commit: %w", err)
	}

	return t, nil
}

/*
┌─ team ──────────────────────────────────────────
│  one team the caller may see, with its statuses
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      Team
│      error                     ErrNotFound
*/

func (s *Service) Get(ctx context.Context, accountID, id string) (Team, error) {
	var t Team

	err := s.pool.QueryRow(ctx,
		`SELECT tm.id::text, tm.workspace_id::text, tm.name, tm.created_at, tm.updated_at
		   FROM team tm
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE tm.id = $1::uuid`,
		id, accountID,
	).Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrNotFound
	}
	if err != nil {
		return Team{}, fmt.Errorf("team: get: %w", err)
	}

	t.Statuses, err = Statuses(ctx, s.pool, t.ID)
	if err != nil {
		return Team{}, err
	}

	return t, nil
}

/*
┌─ team ──────────────────────────────────────────
│  one team's statuses, in position order
├─ in ────────────────────────────────────────────
│      ctx       context.Context
│      q         Querier       pool or transaction
│      teamID    string
├─ out ───────────────────────────────────────────
│      []Status
│      error
*/

// Statuses is exported and takes a Querier because internal/project needs it inside its
// own creation transaction: a seeded board's columns map to the team's statuses, so the
// board bootstrap has to read what the team bootstrap wrote.
//
// Unscoped, like every other read that takes a transaction here - the caller has
// already proved it can reach the team.
func Statuses(ctx context.Context, q Querier, teamID string) ([]Status, error) {
	rows, err := q.Query(ctx,
		`SELECT id::text, name, color, position, category FROM status
		  WHERE team_id = $1::uuid ORDER BY position, id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("team: statuses: %w", err)
	}
	defer rows.Close()

	var out []Status
	for rows.Next() {
		var st Status
		if err := rows.Scan(&st.ID, &st.Name, &st.Color, &st.Position, &st.Category); err != nil {
			return nil, fmt.Errorf("team: statuses scan: %w", err)
		}
		out = append(out, st)
	}

	return out, rows.Err()
}

// Querier is whatever can run a query - the pool, or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func seedStatuses(ctx context.Context, tx pgx.Tx, teamID string) ([]Status, error) {
	out := make([]Status, 0, len(DefaultStatuses))

	for _, want := range DefaultStatuses {
		got := want
		err := tx.QueryRow(ctx,
			`INSERT INTO status (team_id, name, color, position, category)
			 VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text`,
			teamID, want.Name, want.Color, want.Position, want.Category,
		).Scan(&got.ID)
		if err != nil {
			return nil, fmt.Errorf("team: seed status %q: %w", want.Name, err)
		}

		out = append(out, got)
	}

	return out, nil
}

// seedDefaultView writes the team's one starting view.
//
// owner_account_id is deliberately null. A team default belongs to the team rather than
// to whoever happened to create it, and leaving it null also sidesteps LAM-51 entirely:
// there is no owner for an account deletion to orphan.
func seedDefaultView(ctx context.Context, tx pgx.Tx, teamID string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO saved_view (team_id, name, layout, config, is_shared)
		 VALUES ($1::uuid, $2, 'list', $3::jsonb, true)`,
		teamID, defaultViewName, defaultViewConfig)
	if err != nil {
		return fmt.Errorf("team: seed default view: %w", err)
	}

	return nil
}

// isUniqueViolation recognises 23505 without importing pgconn at every call site.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

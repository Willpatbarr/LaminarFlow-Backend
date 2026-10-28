/*
╔═ board.go ════════════════════════════════════════════════════════════════════════════
║  board · boards, their columns, and the statuses each column holds
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service                struct
║      Board                  struct
║      Column                 struct
║      Result                 struct
║      StatusError            struct
║      ErrNotFound            error
║      ErrColumnNotFound      error
║      ErrProjectNotFound     error
║      ErrStatusNotFound      error
║      ErrStatusMapped        error
║      ErrColumnSetWrong      error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/boards · /api/v1/projects/{id}/boards
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package board owns board, board_column and board_column_status (0025, 0026).
//
// A board is project-scoped and addressed by its own id. Its columns are rows with a
// name of their own, a position and a visibility flag, and each column holds any number
// of the team's statuses. Column visibility is stored per board by construction: every
// column belongs to exactly one board, so hiding a column on one board cannot hide it on
// another.
//
// The rule the schema leaves open is that a status sits in one column of a board.
// board_column_status's key is (column, status), so nothing stops the same status being
// mapped into two columns of one board; this package refuses it, and takes a lock on the
// board row for every write that maps a status so two concurrent writes cannot both pass
// the check. project.seedBoard still writes the default board's rows; it maps each status
// once, so it satisfies the same rule.
package board

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxNameLength bounds a board's and a column's name, in characters. Held here and at
// the API, not by a CHECK, for the reason team.MaxNameLength gives.
const MaxNameLength = 100

// GroupByStatus is the only value group_by takes (0025's CHECK).
const GroupByStatus = "status"

var (
	// ErrNotFound collapses absent and out-of-scope, as every service here does.
	ErrNotFound = errors.New("board: not found")

	// ErrColumnNotFound is a column that is absent, or on another board.
	ErrColumnNotFound = errors.New("board: no such column")

	// ErrProjectNotFound is a project that is absent or out of scope.
	ErrProjectNotFound = errors.New("board: no such project")

	// ErrStatusNotFound is a status that is absent or belongs to another team than the
	// board's. One answer for both, so it cannot probe for ids.
	ErrStatusNotFound = errors.New("board: no such status for this board's team")

	// ErrStatusMapped is a status already held by another column of the same board.
	ErrStatusMapped = errors.New("board: status already mapped to another column of this board")

	// ErrColumnSetWrong is a reorder that did not name every column exactly once.
	ErrColumnSetWrong = errors.New("board: the order must name every column of this board exactly once")
)

// StatusError says which entry of status_ids was refused and why. Kind is
// ErrStatusNotFound or ErrStatusMapped.
type StatusError struct {
	Kind  error
	Index int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%v (status_ids[%d])", e.Kind, e.Index) }
func (e *StatusError) Unwrap() error { return e.Kind }

var errGroupBy = errors.New("must be status")

/*
┏━ Board ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one board and its columns
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      ProjectID      string
┃      TeamID         string
┃      WorkspaceID    string      for the change feed
┃      Name           string
┃      GroupBy        string      "status"
┃      Columns        []Column    ordered by position, id
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Rename · Delete
*/

type Board struct {
	ID          string
	ProjectID   string
	TeamID      string
	WorkspaceID string
	Name        string
	GroupBy     string
	Columns     []Column
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

/*
┏━ Column ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one column of a board
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID          string
┃      BoardID     string
┃      Name        string
┃      Position    int         a hint, not a slot
┃      IsVisible   bool
┃      StatusIDs   []string    in the statuses' own order
*/

type Column struct {
	ID        string
	BoardID   string
	Name      string
	Position  int
	IsVisible bool
	StatusIDs []string
}

// Result is what a column write returns: the column as written, and the board it is on
// with every column, for the change feed.
type Result struct {
	Column Column
	Board  Board
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  owner of the board tables
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool    *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · List · Get · Rename · Delete
┃      AddColumn · UpdateColumn · SetColumnStatuses · DeleteColumn · ReorderColumns
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// callerCanReach is the board scoping predicate: the caller is a member of the board's
// team's workspace. $ACCOUNT is filled in by expand, as in internal/aspect.
const callerCanReach = `EXISTS (
	        SELECT 1 FROM team tm
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE tm.id = b.team_id)`

const boardColumns = `b.id::text, b.project_id::text, b.team_id::text,
	(SELECT tm.workspace_id::text FROM team tm WHERE tm.id = b.team_id),
	b.name, b.group_by, b.created_at, b.updated_at`

func (b *Board) dest() []any {
	return []any{&b.ID, &b.ProjectID, &b.TeamID, &b.WorkspaceID, &b.Name, &b.GroupBy, &b.CreatedAt, &b.UpdatedAt}
}

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

/*
┌─ board ─────────────────────────────────────────
│  creates a board on a project the caller can reach
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      projectID    string
│      name         string
│      groupBy      string        "" means status
├─ out ───────────────────────────────────────────
│      Board                      with its columns
│      error                      ErrProjectNotFound · *textlimit.FieldError
*/

// Create seeds the new board the way project.seedBoard seeds the default one: a column
// per status the team has, named and positioned after it and mapped to it. The ticket
// does not say what an additional board starts with, and an empty board shows no cards
// until columns are added by hand; the same start as the default is the least surprising.
func (s *Service) Create(ctx context.Context, accountID, projectID, name, groupBy string) (Board, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Board{}, err
	}
	if groupBy == "" {
		groupBy = GroupByStatus
	}
	if groupBy != GroupByStatus {
		return Board{}, &textlimit.FieldError{Field: "group_by", Reason: errGroupBy}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Board{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var teamID string
	err = tx.QueryRow(ctx,
		`SELECT p.team_id::text FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE p.id = $1::uuid`, projectID, accountID).Scan(&teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, ErrProjectNotFound
	}
	if err != nil {
		return Board{}, fmt.Errorf("board: create scope: %w", err)
	}

	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO board (project_id, team_id, name, group_by)
		 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
		projectID, teamID, name, groupBy).Scan(&id); err != nil {
		return Board{}, fmt.Errorf("board: create: %w", err)
	}

	statuses, err := team.Statuses(ctx, tx, teamID)
	if err != nil {
		return Board{}, err
	}

	// The column row comes first: board_column_status references board_column
	// (id, team_id), so a mapping cannot be written before its column.
	for _, st := range statuses {
		var colID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO board_column (board_id, team_id, name, position)
			 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
			id, teamID, st.Name, st.Position).Scan(&colID); err != nil {
			return Board{}, fmt.Errorf("board: create column %q: %w", st.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO board_column_status (board_column_id, status_id, team_id)
			 VALUES ($1::uuid, $2::uuid, $3::uuid)`, colID, st.ID, teamID); err != nil {
			return Board{}, fmt.Errorf("board: create mapping %q: %w", st.Name, err)
		}
	}

	b, err := s.load(ctx, tx, accountID, id, false)
	if err != nil {
		return Board{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Board{}, fmt.Errorf("board: commit: %w", err)
	}

	return b, nil
}

/*
┌─ board ─────────────────────────────────────────
│  every board of a project, columns included
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      projectID    string
├─ out ───────────────────────────────────────────
│      []Board                    oldest first
│      error                      ErrProjectNotFound
*/

func (s *Service) List(ctx context.Context, accountID, projectID string) ([]Board, error) {
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
		return nil, fmt.Errorf("board: list scope: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+boardColumns+` FROM board b WHERE b.project_id = $1::uuid
		  ORDER BY b.created_at, b.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("board: list: %w", err)
	}
	defer rows.Close()

	var out []Board
	for rows.Next() {
		var b Board
		if err := rows.Scan(b.dest()...); err != nil {
			return nil, fmt.Errorf("board: list scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("board: list rows: %w", err)
	}
	rows.Close()

	if err := attachColumns(ctx, s.pool, out); err != nil {
		return nil, err
	}

	return out, nil
}

// Get returns one board with its columns.
func (s *Service) Get(ctx context.Context, accountID, id string) (Board, error) {
	return s.load(ctx, s.pool, accountID, id, false)
}

// Rename changes the board's name and nothing else.
func (s *Service) Rename(ctx context.Context, accountID, id, name string) (Board, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Board{}, err
	}

	var scanned string
	err := s.pool.QueryRow(ctx,
		expand(`UPDATE board b SET name = $2, updated_at = now()
		         WHERE b.id = $1::uuid AND `+callerCanReach+`
		     RETURNING b.id::text`, "$3"),
		id, name, accountID).Scan(&scanned)
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	if err != nil {
		return Board{}, fmt.Errorf("board: rename: %w", err)
	}

	return s.load(ctx, s.pool, accountID, id, false)
}

// Delete removes a board. Its columns and their mappings cascade, and so do the saved
// views that point at it (0027); no ticket is touched. The last board of a project may
// be deleted - the ticket's default - and a project with no board is legal (0025).
// Returns the board as it was, for the change feed.
func (s *Service) Delete(ctx context.Context, accountID, id string) (Board, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Board{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	b, err := s.load(ctx, tx, accountID, id, true)
	if err != nil {
		return Board{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM board WHERE id = $1::uuid`, id); err != nil {
		return Board{}, fmt.Errorf("board: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Board{}, fmt.Errorf("board: commit: %w", err)
	}

	return b, nil
}

/*
┌─ board ─────────────────────────────────────────
│  appends a column, optionally holding statuses
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      boardID      string
│      name         string
│      statusIDs    []string      may be empty
├─ out ───────────────────────────────────────────
│      Result
│      error        ErrNotFound · *StatusError · *textlimit.FieldError
*/

func (s *Service) AddColumn(ctx context.Context, accountID, boardID, name string, statusIDs []string) (Result, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Result{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	b, err := s.lock(ctx, tx, accountID, boardID)
	if err != nil {
		return Result{}, err
	}

	statusIDs, err = s.checkStatuses(ctx, tx, b, "", statusIDs)
	if err != nil {
		return Result{}, err
	}

	var colID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO board_column (board_id, team_id, name, position)
		 SELECT $1::uuid, $2::uuid, $3,
		        coalesce((SELECT max(c.position) FROM board_column c WHERE c.board_id = $1::uuid), 0) + 1
		 RETURNING id::text`, boardID, b.TeamID, name).Scan(&colID); err != nil {
		return Result{}, fmt.Errorf("board: add column: %w", err)
	}

	if err := mapStatuses(ctx, tx, colID, b.TeamID, statusIDs); err != nil {
		return Result{}, err
	}

	return s.finish(ctx, tx, accountID, boardID, colID)
}

/*
┌─ board ─────────────────────────────────────────
│  renames a column and/or shows or hides it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      boardID      string
│      columnID     string
│      name         *string       nil leaves it alone
│      isVisible    *bool         nil leaves it alone
├─ out ───────────────────────────────────────────
│      Result
│      error        ErrNotFound · ErrColumnNotFound · *textlimit.FieldError
*/

func (s *Service) UpdateColumn(ctx context.Context, accountID, boardID, columnID string, name *string, isVisible *bool) (Result, error) {
	if name != nil {
		if err := textlimit.Required("name", *name, MaxNameLength); err != nil {
			return Result{}, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := s.lock(ctx, tx, accountID, boardID); err != nil {
		return Result{}, err
	}

	if err := s.requireColumn(ctx, tx, boardID, columnID); err != nil {
		return Result{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE board_column SET name = coalesce($3, name), is_visible = coalesce($4, is_visible),
		        updated_at = now()
		  WHERE id = $1::uuid AND board_id = $2::uuid`,
		columnID, boardID, name, isVisible); err != nil {
		return Result{}, fmt.Errorf("board: update column: %w", err)
	}

	return s.finish(ctx, tx, accountID, boardID, columnID)
}

/*
┌─ board ─────────────────────────────────────────
│  replaces the set of statuses a column holds
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      boardID      string
│      columnID     string
│      statusIDs    []string      the full set; empty unmaps all
├─ out ───────────────────────────────────────────
│      Result
│      error        ErrNotFound · ErrColumnNotFound · *StatusError
*/

// SetColumnStatuses is the full set rather than an add and a remove, so the result is
// exactly what was sent. A status already held by another column of this board is
// refused - move it by sending the other column's set first.
func (s *Service) SetColumnStatuses(ctx context.Context, accountID, boardID, columnID string, statusIDs []string) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	b, err := s.lock(ctx, tx, accountID, boardID)
	if err != nil {
		return Result{}, err
	}

	if err := s.requireColumn(ctx, tx, boardID, columnID); err != nil {
		return Result{}, err
	}

	statusIDs, err = s.checkStatuses(ctx, tx, b, columnID, statusIDs)
	if err != nil {
		return Result{}, err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM board_column_status
		  WHERE board_column_id = $1::uuid AND NOT (status_id = ANY($2::uuid[]))`,
		columnID, statusIDs); err != nil {
		return Result{}, fmt.Errorf("board: unmap statuses: %w", err)
	}

	if err := mapStatuses(ctx, tx, columnID, b.TeamID, statusIDs); err != nil {
		return Result{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE board_column SET updated_at = now() WHERE id = $1::uuid`, columnID); err != nil {
		return Result{}, fmt.Errorf("board: touch column: %w", err)
	}

	return s.finish(ctx, tx, accountID, boardID, columnID)
}

/*
┌─ board ─────────────────────────────────────────
│  deletes a column, unmapping its statuses
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      boardID      string
│      columnID     string
├─ out ───────────────────────────────────────────
│      Result                     Column as it was, Board without it
│      error                      ErrNotFound · ErrColumnNotFound
*/

// DeleteColumn leaves tickets alone: board_column_status cascades and nothing else
// references the column. The statuses it held are simply on no column until mapped again.
func (s *Service) DeleteColumn(ctx context.Context, accountID, boardID, columnID string) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := s.lock(ctx, tx, accountID, boardID); err != nil {
		return Result{}, err
	}

	b, err := s.load(ctx, tx, accountID, boardID, false)
	if err != nil {
		return Result{}, err
	}

	var gone *Column
	for i := range b.Columns {
		if b.Columns[i].ID == columnID {
			gone = &b.Columns[i]
		}
	}
	if gone == nil {
		return Result{}, ErrColumnNotFound
	}
	res := Result{Column: *gone}

	if _, err := tx.Exec(ctx,
		`DELETE FROM board_column WHERE id = $1::uuid AND board_id = $2::uuid`, columnID, boardID); err != nil {
		return Result{}, fmt.Errorf("board: delete column: %w", err)
	}

	if res.Board, err = s.load(ctx, tx, accountID, boardID, false); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("board: commit: %w", err)
	}

	return res, nil
}

/*
┌─ board ─────────────────────────────────────────
│  rewrites every column's position in one statement
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      boardID      string
│      columnIDs    []string      the whole set, in order
├─ out ───────────────────────────────────────────
│      Board
│      error        ErrNotFound · ErrColumnSetWrong
├─ example ───────────────────────────────────────
│      [c, a, b]  →  positions 1, 2, 3
*/

// ReorderColumns takes the complete ordered set for the reason aspect.Reorder gives: a
// partial list would renumber some rows and leave others, which corrupts an ordering
// rather than failing. The set check runs in the transaction that writes.
func (s *Service) ReorderColumns(ctx context.Context, accountID, boardID string, columnIDs []string) (Board, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Board{}, fmt.Errorf("board: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := s.lock(ctx, tx, accountID, boardID); err != nil {
		return Board{}, err
	}

	cur, err := columnsOf(ctx, tx, boardID)
	if err != nil {
		return Board{}, err
	}

	want := make(map[string]bool, len(cur))
	for _, c := range cur {
		want[c.ID] = true
	}
	if len(columnIDs) != len(cur) {
		return Board{}, ErrColumnSetWrong
	}
	for _, id := range columnIDs {
		if !uuidRE.MatchString(id) || !want[strings.ToLower(id)] {
			return Board{}, ErrColumnSetWrong
		}
		delete(want, strings.ToLower(id))
	}

	if _, err := tx.Exec(ctx,
		`UPDATE board_column c SET position = o.ord, updated_at = now()
		   FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, ord)
		  WHERE c.id = o.id AND c.board_id = $1::uuid`, boardID, columnIDs); err != nil {
		return Board{}, fmt.Errorf("board: reorder: %w", err)
	}

	out, err := s.load(ctx, tx, accountID, boardID, false)
	if err != nil {
		return Board{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Board{}, fmt.Errorf("board: commit: %w", err)
	}

	return out, nil
}

// lock proves the caller can reach the board and takes its row lock, which serialises
// every write that changes what the board's columns hold.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, accountID, id string) (Board, error) {
	var b Board
	err := tx.QueryRow(ctx,
		expand(`SELECT `+boardColumns+` FROM board b
		         WHERE b.id = $1::uuid AND `+callerCanReach+` FOR UPDATE OF b`, "$2"),
		id, accountID).Scan(b.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	if err != nil {
		return Board{}, fmt.Errorf("board: lock: %w", err)
	}

	return b, nil
}

func (s *Service) requireColumn(ctx context.Context, tx pgx.Tx, boardID, columnID string) error {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM board_column WHERE id = $1::uuid AND board_id = $2::uuid`,
		columnID, boardID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrColumnNotFound
	}
	if err != nil {
		return fmt.Errorf("board: column: %w", err)
	}

	return nil
}

// checkStatuses refuses a status that is not the board's team's, or that another column
// of this board already holds. columnID is the column being written, "" for a new one.
// Returns the ids with duplicates removed, in the order sent, lower-cased.
func (s *Service) checkStatuses(ctx context.Context, tx pgx.Tx, b Board, columnID string, ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if !uuidRE.MatchString(id) {
			return nil, &StatusError{Kind: ErrStatusNotFound, Index: i}
		}
		id = strings.ToLower(id)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}

	known := map[string]bool{}
	rows, err := tx.Query(ctx,
		`SELECT id::text FROM status WHERE team_id = $1::uuid AND id = ANY($2::uuid[])`, b.TeamID, out)
	if err != nil {
		return nil, fmt.Errorf("board: statuses: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("board: statuses scan: %w", err)
		}
		known[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("board: statuses rows: %w", err)
	}

	for i, id := range ids {
		if !known[strings.ToLower(id)] {
			return nil, &StatusError{Kind: ErrStatusNotFound, Index: i}
		}
	}

	held := map[string]bool{}
	rows, err = tx.Query(ctx,
		`SELECT bcs.status_id::text
		   FROM board_column_status bcs
		   JOIN board_column c ON c.id = bcs.board_column_id
		  WHERE c.board_id = $1::uuid AND ($2::text = '' OR c.id::text <> $2::text)
		    AND bcs.status_id = ANY($3::uuid[])`, b.ID, columnID, out)
	if err != nil {
		return nil, fmt.Errorf("board: mapped: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("board: mapped scan: %w", err)
		}
		held[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("board: mapped rows: %w", err)
	}

	for i, id := range ids {
		if held[strings.ToLower(id)] {
			return nil, &StatusError{Kind: ErrStatusMapped, Index: i}
		}
	}

	return out, nil
}

func mapStatuses(ctx context.Context, tx pgx.Tx, columnID, teamID string, statusIDs []string) error {
	if len(statusIDs) == 0 {
		return nil
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO board_column_status (board_column_id, status_id, team_id)
		 SELECT $1::uuid, s, $3::uuid FROM unnest($2::uuid[]) AS s
		 ON CONFLICT (board_column_id, status_id) DO NOTHING`,
		columnID, statusIDs, teamID); err != nil {
		return fmt.Errorf("board: map statuses: %w", err)
	}

	return nil
}

// finish reads the written column and its board back in the writing transaction, commits,
// and returns both.
func (s *Service) finish(ctx context.Context, tx pgx.Tx, accountID, boardID, columnID string) (Result, error) {
	b, err := s.load(ctx, tx, accountID, boardID, false)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, c := range b.Columns {
		if c.ID == columnID {
			res = Result{Column: c, Board: b}
		}
	}
	if res.Column.ID == "" {
		return Result{}, fmt.Errorf("board: column %s vanished inside its own transaction", columnID)
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("board: commit: %w", err)
	}

	return res, nil
}

// load reads one scoped board with its columns.
func (s *Service) load(ctx context.Context, q querier, accountID, id string, forUpdate bool) (Board, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF b"
	}

	var b Board
	err := q.QueryRow(ctx,
		expand(`SELECT `+boardColumns+` FROM board b
		         WHERE b.id = $1::uuid AND `+callerCanReach+lock, "$2"),
		id, accountID).Scan(b.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Board{}, ErrNotFound
	}
	if err != nil {
		return Board{}, fmt.Errorf("board: get: %w", err)
	}

	boards := []Board{b}
	if err := attachColumns(ctx, q, boards); err != nil {
		return Board{}, err
	}

	return boards[0], nil
}

func columnsOf(ctx context.Context, q querier, boardID string) ([]Column, error) {
	b := []Board{{ID: boardID}}
	if err := attachColumns(ctx, q, b); err != nil {
		return nil, err
	}

	return b[0].Columns, nil
}

// attachColumns fills Columns on every board in two statements: the columns, then their
// statuses. Both are ordered as the tables define - (position, id) for columns, and the
// status's own (position, id) for the ids - so a board reads the same through any path.
func attachColumns(ctx context.Context, q querier, boards []Board) error {
	if len(boards) == 0 {
		return nil
	}

	ids := make([]string, len(boards))
	at := make(map[string]int, len(boards))
	for i := range boards {
		ids[i] = boards[i].ID
		at[boards[i].ID] = i
		boards[i].Columns = []Column{}
	}

	rows, err := q.Query(ctx,
		`SELECT c.id::text, c.board_id::text, c.name, c.position, c.is_visible
		   FROM board_column c WHERE c.board_id = ANY($1::uuid[])
		  ORDER BY c.board_id, c.position, c.id`, ids)
	if err != nil {
		return fmt.Errorf("board: columns: %w", err)
	}

	col := map[string][2]int{}
	for rows.Next() {
		c := Column{StatusIDs: []string{}}
		if err := rows.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &c.IsVisible); err != nil {
			rows.Close()
			return fmt.Errorf("board: columns scan: %w", err)
		}
		bi := at[c.BoardID]
		col[c.ID] = [2]int{bi, len(boards[bi].Columns)}
		boards[bi].Columns = append(boards[bi].Columns, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("board: columns rows: %w", err)
	}

	rows, err = q.Query(ctx,
		`SELECT bcs.board_column_id::text, bcs.status_id::text
		   FROM board_column_status bcs
		   JOIN board_column c ON c.id = bcs.board_column_id
		   JOIN status s ON s.id = bcs.status_id
		  WHERE c.board_id = ANY($1::uuid[])
		  ORDER BY s.position, s.id`, ids)
	if err != nil {
		return fmt.Errorf("board: column statuses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var colID, statusID string
		if err := rows.Scan(&colID, &statusID); err != nil {
			return fmt.Errorf("board: column statuses scan: %w", err)
		}
		p := col[colID]
		c := &boards[p[0]].Columns[p[1]]
		c.StatusIDs = append(c.StatusIDs, statusID)
	}

	return rows.Err()
}

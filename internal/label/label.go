/*
╔═ label.go ════════════════════════════════════════════════════════════════════════════
║  label · team-scoped CRUD
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service            struct
║      Label              struct
║      ErrNotFound        error
║      ErrTeamNotFound    error
║      ErrNameTaken       error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/labels
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package label owns the label table (0023): a team's vocabulary of coloured tags.
//
// Which tickets carry a label is ticket_label (0024), written with the ticket - a
// ticket's labels are part of the ticket, so changing them moves its version and is
// announced as ticket.updated. Deleting a label cascades its ticket_label rows away
// without touching the tickets' rows, so the change feed's label.deleted is how a
// client learns to drop it from the tickets it holds.
package label

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
	ErrNotFound = errors.New("label: not found")

	// ErrTeamNotFound is a team that is absent or out of scope.
	ErrTeamNotFound = errors.New("label: no such team")

	// ErrNameTaken is a name the team already has: 0023's UNIQUE (team_id, name), by name.
	ErrNameTaken = errors.New("label: name already used in this team")
)

// The longest a label's text may be, in characters. The colour is whatever string the
// client likes, up to this many: nothing here reads it, as with status.color.
const (
	MaxNameLength  = 100
	MaxColorLength = 32
)

const nameKey = "label_team_name_key"

/*
┏━ Label ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one label as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      TeamID         string
┃      WorkspaceID    string      for the change feed
┃      Name           string
┃      Color          string
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Update · Delete
*/

type Label struct {
	ID          string
	TeamID      string
	WorkspaceID string
	Name        string
	Color       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Patch is an update. A nil member is left alone.
type Patch struct {
	Name  *string
	Color *string
}

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const callerCanReach = `EXISTS (
	        SELECT 1 FROM team tm
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE tm.id = l.team_id)`

const columns = `l.id::text, l.team_id::text,
	(SELECT tm.workspace_id::text FROM team tm WHERE tm.id = l.team_id),
	l.name, l.color, l.created_at, l.updated_at`

func (l *Label) dest() []any {
	return []any{&l.ID, &l.TeamID, &l.WorkspaceID, &l.Name, &l.Color, &l.CreatedAt, &l.UpdatedAt}
}

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

// writeError maps the unique violation; nil for anything else.
func writeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == nameKey {
		return ErrNameTaken
	}

	return nil
}

// Create inserts a label into a team the caller can reach.
func (s *Service) Create(ctx context.Context, accountID, teamID, name, color string) (Label, error) {
	if err := textlimit.Required("name", name, MaxNameLength); err != nil {
		return Label{}, err
	}
	if err := textlimit.Required("color", color, MaxColorLength); err != nil {
		return Label{}, err
	}

	var l Label
	err := s.pool.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO label (team_id, name, color)
		     SELECT tm.id, $2, $3
		       FROM team tm
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $4::uuid
		      WHERE tm.id = $1::uuid
		     RETURNING *)
		 SELECT `+columns+` FROM ins l`,
		teamID, name, color, accountID,
	).Scan(l.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Label{}, ErrTeamNotFound
	}
	if e := writeError(err); e != nil {
		return Label{}, e
	}
	if err != nil {
		return Label{}, fmt.Errorf("label: insert: %w", err)
	}

	return l, nil
}

// List is every label of a team, by name. One page, always: a team's vocabulary is a
// few dozen words, so there is nothing to filter or page.
func (s *Service) List(ctx context.Context, accountID, teamID string) ([]Label, error) {
	var reachable string
	err := s.pool.QueryRow(ctx,
		`SELECT tm.id::text FROM team tm
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		  WHERE tm.id = $1::uuid`, teamID, accountID).Scan(&reachable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTeamNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("label: list scope: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM label l WHERE l.team_id = $1::uuid ORDER BY l.name, l.id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("label: list: %w", err)
	}
	defer rows.Close()

	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(l.dest()...); err != nil {
			return nil, fmt.Errorf("label: list scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("label: list rows: %w", err)
	}

	return out, nil
}

// Get returns one label the caller may see.
func (s *Service) Get(ctx context.Context, accountID, id string) (Label, error) {
	var l Label
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM label l WHERE l.id = $1::uuid AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(l.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Label{}, ErrNotFound
	}
	if err != nil {
		return Label{}, fmt.Errorf("label: get: %w", err)
	}

	return l, nil
}

// Update applies a patch; a nil member keeps its value.
func (s *Service) Update(ctx context.Context, accountID, id string, p Patch) (Label, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxNameLength); err != nil {
			return Label{}, err
		}
	}
	if p.Color != nil {
		if err := textlimit.Required("color", *p.Color, MaxColorLength); err != nil {
			return Label{}, err
		}
	}

	var l Label
	err := s.pool.QueryRow(ctx,
		expand(`WITH upd AS (
		     UPDATE label l
		        SET name = coalesce($2::text, l.name),
		            color = coalesce($3::text, l.color),
		            updated_at = now()
		      WHERE l.id = $1::uuid AND `+callerCanReach+`
		     RETURNING l.*)
		 SELECT `+columns+` FROM upd l`, "$4"),
		id, p.Name, p.Color, accountID,
	).Scan(l.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Label{}, ErrNotFound
	}
	if e := writeError(err); e != nil {
		return Label{}, e
	}
	if err != nil {
		return Label{}, fmt.Errorf("label: update: %w", err)
	}

	return l, nil
}

// Delete removes a label and returns it as it was, for the change feed. The ticket_label
// rows cascade away (0024); the tickets stay.
func (s *Service) Delete(ctx context.Context, accountID, id string) (Label, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Label{}, fmt.Errorf("label: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var l Label
	err = tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM label l WHERE l.id = $1::uuid AND `+callerCanReach+` FOR UPDATE OF l`, "$2"),
		id, accountID,
	).Scan(l.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Label{}, ErrNotFound
	}
	if err != nil {
		return Label{}, fmt.Errorf("label: delete lookup: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM label WHERE id = $1::uuid`, id); err != nil {
		return Label{}, fmt.Errorf("label: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Label{}, fmt.Errorf("label: commit: %w", err)
	}

	return l, nil
}

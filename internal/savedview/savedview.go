/*
╔═ savedview.go ════════════════════════════════════════════════════════════════════════
║  savedview · CRUD and list for saved views
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service                struct
║      View                   struct
║      Config                 struct
║      CreateParams           struct
║      Patch                  struct
║      ListParams             struct
║      Page                   struct
║      Fields                 filter.Schema
║      ErrNotFound           error
║      ErrNotOwner           error
║      ErrTeamNotFound       error
║      ErrProjectNotFound    error
║      ErrBoardNotFound      error
║      ErrBoardRequired      error
║      ErrBoardForbidden     error
║      ErrLayoutNeedsBoard   error
║      ErrOwnerless          error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/saved-views
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package savedview owns the saved_view table (0027): a configured list or board that a
// person keeps, privately or shared with their team (Master Spec 3.7).
//
// A view's config is jsonb, so the schema proves only that it is an object. Everything
// inside it - the filter, the grouping, the sort and the visible columns - is validated
// here, against the ticket vocabulary, because a saved view filters tickets. The filter
// is the same language POST /tickets/list takes (LAM-58), compiled with the same
// schema, so a filter that is saved is a filter that will run.
//
// Visibility: a private view is visible to its owner only, a shared one to everyone who
// can reach its team. Another person's private view is not found, never forbidden.
//
// Editing: the owner edits and deletes. A view with no owner is a team's own - the
// default view team creation seeds, or a shared view whose author's account was deleted
// (LAM-51) - and any member who can reach the team may edit or delete it, because
// nobody owns it and "nothing seeded is privileged" (create-team) would otherwise be
// untrue. Such a view cannot be made private: it would be owned by and visible to
// nobody, which is the state LAM-51 exists to keep unreachable.
package savedview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxNameLength bounds a view's name, in characters.
const MaxNameLength = 100

// Layouts are saved_view_layout_is_known's closed set.
const (
	LayoutList  = "list"
	LayoutBoard = "board"
)

var (
	// ErrNotFound collapses absent, out of scope, and another person's private view.
	ErrNotFound = errors.New("savedview: not found")

	// ErrNotOwner is a view the caller can see and does not own.
	ErrNotOwner = errors.New("savedview: not the owner")

	// ErrTeamNotFound is a team that is absent or outside the caller's workspace.
	ErrTeamNotFound = errors.New("savedview: no such team")

	// ErrProjectNotFound is a project that is absent or not in the view's team.
	ErrProjectNotFound = errors.New("savedview: no such project in this team")

	// ErrBoardNotFound is a board that is absent, not in the view's team, or not in
	// the view's project when it has one.
	ErrBoardNotFound = errors.New("savedview: no such board")

	// ErrBoardRequired: a board view without a board_id.
	ErrBoardRequired = errors.New("savedview: a board view needs a board_id")

	// ErrBoardForbidden: a list view with a board_id.
	ErrBoardForbidden = errors.New("savedview: a list view must not have a board_id")

	// ErrLayoutNeedsBoard: an update turning a list view into a board view. An update
	// cannot name a board, so it cannot make that change.
	ErrLayoutNeedsBoard = errors.New("savedview: a list view cannot become a board view")

	// ErrOwnerless: an update making a team's own view private.
	ErrOwnerless = errors.New("savedview: a view with no owner cannot be made private")

	errLayout = errors.New("must be list or board")
)

/*
┏━ Config ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what a view remembers besides where it lives
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Filter     *filter.Group    nil means no filter
┃      Group      string           a ticket field name, "" means ungrouped
┃      Sort       []filter.Order   empty means the list's default
┃      Columns    []string         ticket field names, empty means the client's default
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      JSON decoding of a request body or saved_view.config
*/

// Config is persisted: saved_view.config holds it, so renaming a member is a data
// migration and adding one is free. The team default is written as a literal in
// internal/team for the same reason, and decodes into this.
//
// The blank field is how huma is told to refuse members this struct does not have. A
// typo'd "sorts" would otherwise be stored, read back as nothing, and look like a
// view that forgot its sort.
type Config struct {
	_ struct{} `additionalProperties:"false"`

	Filter  *filter.Group  `json:"filter,omitempty" doc:"The filter language of POST /tickets/list, over the ticket vocabulary. Omit for none."`
	Group   string         `json:"group,omitempty" doc:"A ticket filter field to group by. Multi-valued fields (label, sprint) cannot be grouped. Omit for ungrouped."`
	Sort    []filter.Order `json:"sort,omitempty" doc:"Sort keys, most significant first, over the ticket vocabulary."`
	Columns []string       `json:"columns,omitempty" doc:"The visible columns, in order, by ticket field name. Each at most once."`
}

/*
┏━ View ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one saved view as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID, TeamID             string
┃      ProjectID, BoardID     *string    null when team-wide / a list view
┃      OwnerAccountID         *string    null for a team's own view
┃      Name, Layout           string
┃      Config                 Config
┃      IsShared               bool
┃      CreatedAt, UpdatedAt   time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · List · Update
*/

type View struct {
	ID             string
	TeamID         string
	ProjectID      *string
	BoardID        *string
	OwnerAccountID *string
	Name           string
	Layout         string
	Config         Config
	IsShared       bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CreateParams is a new view. A nil Config is an empty one.
type CreateParams struct {
	TeamID    string
	ProjectID *string
	BoardID   *string
	Name      string
	Layout    string
	Config    Config
	IsShared  bool
}

// Patch is an update. A nil member is left alone; a Config replaces the whole config.
type Patch struct {
	Name     *string
	Layout   *string
	Config   *Config
	IsShared *bool
}

// Service reads and writes saved views.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service over pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Fields is the vocabulary the list endpoint sorts and pages on. It is not the ticket
// vocabulary: that one belongs to Config.
var Fields = filter.Schema{
	"name":       {Expr: "sv.name", Kind: filter.Text},
	"created_at": {Expr: "sv.created_at", Kind: filter.Time},
	"updated_at": {Expr: "sv.updated_at", Kind: filter.Time},
}

// Tiebreak completes the list's order; it is the primary key, so the order is total.
const Tiebreak = "sv.id"

// DefaultSort is name, ascending: what a sidebar shows.
var DefaultSort = []filter.Order{{Field: "name"}}

// reach is the caller being in the workspace of the view's team. $ACCOUNT is replaced
// by expand.
const reach = `EXISTS (
	        SELECT 1 FROM team tm
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE tm.id = sv.team_id)`

// visible is reach plus the private rule: a private view is its owner's alone.
const visible = reach + ` AND (sv.is_shared OR sv.owner_account_id = $ACCOUNT::uuid)`

const columns = `sv.id::text, sv.team_id::text, sv.project_id::text, sv.board_id::text,
	sv.owner_account_id::text, sv.name, sv.layout, sv.config, sv.is_shared, sv.created_at, sv.updated_at`

func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

func scanDest(v *View, raw *[]byte) []any {
	return []any{&v.ID, &v.TeamID, &v.ProjectID, &v.BoardID, &v.OwnerAccountID, &v.Name, &v.Layout,
		raw, &v.IsShared, &v.CreatedAt, &v.UpdatedAt}
}

func decodeConfig(v *View, raw []byte) error {
	if err := json.Unmarshal(raw, &v.Config); err != nil {
		return fmt.Errorf("savedview: config of %s: %w", v.ID, err)
	}

	return nil
}

/*
┌─ savedview ─────────────────────────────────────
│  checks a config against the ticket vocabulary
├─ in ────────────────────────────────────────────
│      c    Config
├─ out ───────────────────────────────────────────
│      error    *filter.Error, every fault located at body.config...
├─ example ───────────────────────────────────────
│      filter on "nope"  →  body.config.filter.conditions[0].field
*/

// Validate runs the filter through the compiler that will run it, the sort through the
// ORDER BY builder, and checks group and columns against the same schema - so nothing
// can be saved that the ticket list would refuse.
//
// Every fault is collected, none short-circuits, and locations are absolute against the
// request body ("body.config...") because a view's config sits there on both create and
// update.
func (c Config) Validate() error {
	var faults []Fault

	if c.Filter != nil {
		g := *c.Filter
		// The same normalisation filter.Schema.Plan applies: an empty group with no
		// op is "no filter", not a malformed one.
		if g.Op == "" && len(g.Conditions) == 0 && len(g.Groups) == 0 {
			g.Op = filter.And
		}

		if _, _, err := ticket.Fields.Compile(g, 1); err != nil {
			faults = append(faults, relocate(err, "body.config.filter")...)
		}
	}

	if len(c.Sort) > 0 {
		if _, err := ticket.Fields.CompileOrder(c.Sort, ticket.Tiebreak); err != nil {
			faults = append(faults, relocate(err, "body.config")...)
		}
	}

	if c.Group != "" {
		f, ok := ticket.Fields[c.Group]
		switch {
		case !ok:
			faults = append(faults, Fault{Location: "body.config.group", Message: "unknown field", Value: c.Group})
		case f.Kind == filter.UUIDSet:
			faults = append(faults, Fault{Location: "body.config.group", Message: "not groupable", Value: c.Group})
		}
	}

	seen := map[string]bool{}
	for i, name := range c.Columns {
		at := "body.config.columns[" + strconv.Itoa(i) + "]"

		if _, ok := ticket.Fields[name]; !ok {
			faults = append(faults, Fault{Location: at, Message: "unknown field", Value: name})
			continue
		}
		if seen[name] {
			faults = append(faults, Fault{Location: at, Message: "listed twice", Value: name})
		}
		seen[name] = true
	}

	if len(faults) > 0 {
		return &filter.Error{Faults: faults}
	}

	return nil
}

// Fault is filter.Fault, named here so the faults above read as what they are.
type Fault = filter.Fault

// relocate prefixes the locations of a filter.Error. A fault with no location sits at
// the prefix itself.
func relocate(err error, at string) []Fault {
	var fe *filter.Error
	if !errors.As(err, &fe) {
		return []Fault{{Location: at, Message: err.Error()}}
	}

	out := make([]Fault, len(fe.Faults))
	for i, f := range fe.Faults {
		out[i] = f
		if f.Location == "" {
			out[i].Location = at
			continue
		}
		out[i].Location = at + "." + f.Location
	}

	return out
}

func validLayout(layout string) error {
	if layout != LayoutList && layout != LayoutBoard {
		return &textlimit.FieldError{Field: "layout", Reason: errLayout}
	}

	return nil
}

func marshal(c Config) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("savedview: marshal config: %w", err)
	}

	return raw, nil
}

/*
┌─ savedview ─────────────────────────────────────
│  creates a view owned by the caller
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      View
│      error        *textlimit.FieldError · *filter.Error · Err…
├─ example ───────────────────────────────────────
│      private list view  →  View{OwnerAccountID: caller}
*/

// Create checks the request before it reads the database, cheapest first, then resolves
// the team (404), the project and the board (422 at their own fields).
//
// The foreign keys of 0027 would refuse a project or board of another team on their
// own, but as a 23503 - a 500. Checking here is what turns it into the 422 the field
// deserves.
func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (View, error) {
	if err := textlimit.Required("name", p.Name, MaxNameLength); err != nil {
		return View{}, err
	}
	if err := validLayout(p.Layout); err != nil {
		return View{}, err
	}
	if p.Layout == LayoutBoard && p.BoardID == nil {
		return View{}, ErrBoardRequired
	}
	if p.Layout == LayoutList && p.BoardID != nil {
		return View{}, ErrBoardForbidden
	}
	if err := p.Config.Validate(); err != nil {
		return View{}, err
	}

	raw, err := marshal(p.Config)
	if err != nil {
		return View{}, err
	}

	var reachable bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM team tm
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		      WHERE tm.id = $1::uuid)`, p.TeamID, accountID).Scan(&reachable); err != nil {
		return View{}, fmt.Errorf("savedview: create scope: %w", err)
	}
	if !reachable {
		return View{}, ErrTeamNotFound
	}

	if p.ProjectID != nil {
		var ok bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM project WHERE id = $1::uuid AND team_id = $2::uuid)`,
			*p.ProjectID, p.TeamID).Scan(&ok); err != nil {
			return View{}, fmt.Errorf("savedview: create project: %w", err)
		}
		if !ok {
			return View{}, ErrProjectNotFound
		}
	}

	if p.BoardID != nil {
		// A board of the view's team, and of its project when it names one - the same
		// two references saved_view_board_team_fkey and saved_view_board_project_fkey
		// make.
		var ok bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM board
			                 WHERE id = $1::uuid AND team_id = $2::uuid
			                   AND ($3::uuid IS NULL OR project_id = $3::uuid))`,
			*p.BoardID, p.TeamID, p.ProjectID).Scan(&ok); err != nil {
			return View{}, fmt.Errorf("savedview: create board: %w", err)
		}
		if !ok {
			return View{}, ErrBoardNotFound
		}
	}

	var (
		v   View
		out []byte
	)
	err = s.pool.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO saved_view (team_id, project_id, owner_account_id, board_id, name, layout, config, is_shared)
		     VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7::jsonb, $8)
		     RETURNING *)
		 SELECT `+columns+` FROM ins sv`,
		p.TeamID, p.ProjectID, accountID, p.BoardID, p.Name, p.Layout, raw, p.IsShared,
	).Scan(scanDest(&v, &out)...)
	if err != nil {
		return View{}, fmt.Errorf("savedview: insert: %w", err)
	}

	return v, decodeConfig(&v, out)
}

// Get returns one view the caller may see: their own, or a shared one in a team they
// can reach.
func (s *Service) Get(ctx context.Context, accountID, id string) (View, error) {
	var (
		v   View
		raw []byte
	)
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM saved_view sv WHERE sv.id = $1::uuid AND `+visible, "$2"),
		id, accountID,
	).Scan(scanDest(&v, &raw)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("savedview: get: %w", err)
	}

	return v, decodeConfig(&v, raw)
}

/*
┏━ ListParams ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what one page request asks for
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      TeamID       string
┃      ProjectID    *string    nil means every view of the team
┃      Limit        int        0 means filter.DefaultLimit
┃      Cursor       string     "" means the first page
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api  →  the list request body
*/

type ListParams struct {
	TeamID    string
	ProjectID *string
	Limit     int
	Cursor    string
}

// Page is one page and how to ask for the next. No total, for the reason ticket.Page
// gives.
type Page struct {
	Views      []View
	NextCursor string
	HasMore    bool
}

// List is the caller's own private views plus every shared view of the team, by name.
//
// With a project, the project's views and the team-wide ones (a null project_id): a
// project's sidebar wants both, and a project-scoped view never appears under another
// project. Without one, every view of the team.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	// No filter, no sort of the caller's: the body offers neither, so this is the
	// plan for the default order, the limit bound and the cursor check.
	plan, err := Fields.Plan(filter.Request{Limit: p.Limit, Cursor: p.Cursor}, Tiebreak, DefaultSort, 4)
	if err != nil {
		return Page{}, err
	}

	var reachable bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM team tm
		       JOIN workspace_member wm
		            ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		      WHERE tm.id = $1::uuid)`, p.TeamID, accountID).Scan(&reachable); err != nil {
		return Page{}, fmt.Errorf("savedview: list scope: %w", err)
	}
	if !reachable {
		return Page{}, ErrTeamNotFound
	}

	args := append([]any{accountID, p.TeamID, p.ProjectID}, plan.Args...)
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+`, `+strings.Join(plan.Keys, ", ")+`
		   FROM saved_view sv
		  WHERE sv.team_id = $2::uuid
		    AND (sv.is_shared OR sv.owner_account_id = $1::uuid)
		    AND ($3::uuid IS NULL OR sv.project_id IS NULL OR sv.project_id = $3::uuid)
		    AND (`+plan.Where+`) `+plan.Order+` LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return Page{}, fmt.Errorf("savedview: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)

	for rows.Next() {
		var (
			v   View
			raw []byte
			key = make([]*string, len(plan.Keys))
		)

		dest := scanDest(&v, &raw)
		for i := range key {
			dest = append(dest, &key[i])
		}

		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("savedview: list scan: %w", err)
		}

		if len(page.Views) == plan.Limit {
			// The sentinel row: its existence is the whole answer.
			page.HasMore = true
			break
		}

		if err := decodeConfig(&v, raw); err != nil {
			return Page{}, err
		}

		page.Views = append(page.Views, v)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("savedview: list rows: %w", err)
	}

	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

// lock reads a view for a write: visible to the caller, locked, and editable by them.
func lock(ctx context.Context, tx pgx.Tx, accountID, id string) (View, error) {
	var (
		v   View
		raw []byte
	)
	err := tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM saved_view sv
		         WHERE sv.id = $1::uuid AND `+visible+` FOR UPDATE OF sv`, "$2"),
		id, accountID,
	).Scan(scanDest(&v, &raw)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("savedview: lock: %w", err)
	}

	// A view with an owner is that owner's to change. One without is the team's.
	if v.OwnerAccountID != nil && *v.OwnerAccountID != accountID {
		return View{}, ErrNotOwner
	}

	return v, decodeConfig(&v, raw)
}

// Update applies a patch; a member it leaves alone keeps its value. A config replaces
// the old one whole rather than merging, so a client can drop a sort by omitting it.
//
// Turning a board view into a list view clears board_id, because
// saved_view_board_matches_layout allows nothing else. Turning a list view into a board
// view is refused: the body has no board_id to give it.
func (s *Service) Update(ctx context.Context, accountID, id string, p Patch) (View, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxNameLength); err != nil {
			return View{}, err
		}
	}
	if p.Layout != nil {
		if err := validLayout(*p.Layout); err != nil {
			return View{}, err
		}
	}
	if p.Config != nil {
		if err := p.Config.Validate(); err != nil {
			return View{}, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return View{}, fmt.Errorf("savedview: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	cur, err := lock(ctx, tx, accountID, id)
	if err != nil {
		return View{}, err
	}

	name, layout, cfg, shared := cur.Name, cur.Layout, cur.Config, cur.IsShared
	boardID := cur.BoardID

	if p.Name != nil {
		name = *p.Name
	}
	if p.Config != nil {
		cfg = *p.Config
	}
	if p.IsShared != nil {
		shared = *p.IsShared
	}
	if p.Layout != nil && *p.Layout != layout {
		if *p.Layout == LayoutBoard {
			return View{}, ErrLayoutNeedsBoard
		}
		layout, boardID = LayoutList, nil
	}
	if !shared && cur.OwnerAccountID == nil {
		return View{}, ErrOwnerless
	}

	raw, err := marshal(cfg)
	if err != nil {
		return View{}, err
	}

	var (
		v   View
		out []byte
	)
	err = tx.QueryRow(ctx,
		`WITH upd AS (
		     UPDATE saved_view sv
		        SET name = $2, layout = $3, board_id = $4::uuid, config = $5::jsonb,
		            is_shared = $6, updated_at = now()
		      WHERE sv.id = $1::uuid
		     RETURNING *)
		 SELECT `+columns+` FROM upd sv`,
		id, name, layout, boardID, raw, shared,
	).Scan(scanDest(&v, &out)...)
	if err != nil {
		return View{}, fmt.Errorf("savedview: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("savedview: commit: %w", err)
	}

	return v, decodeConfig(&v, out)
}

// Delete removes a view the caller owns (or, for a team's own view, any member).
func (s *Service) Delete(ctx context.Context, accountID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savedview: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := lock(ctx, tx, accountID, id); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM saved_view WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("savedview: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("savedview: commit: %w", err)
	}

	return nil
}

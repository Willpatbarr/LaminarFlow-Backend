/*
╔═ ticket.go ═══════════════════════════════════════════════════════════════════════════
║  ticket · the CRUD pattern every resource copies
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service        struct
║      Ticket         struct
║      CreateParams   struct
║      UpdateParams   struct
║      ErrNotFound    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  create · get · update · archive
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package ticket owns the ticket table.
//
// It is the first resource service, so it is also the template: unexported pool, one
// exported method per verb, every method scoped by the caller, and no SQL anywhere
// else. internal/document is the shape it mirrors; sqlguard enforces the ownership.
//
// Two rules here are not obvious and are easy to drop when copying this for the next
// resource. Every statement carries archived_at IS NULL, and every statement carries
// the caller's membership join - both live in the constants below rather than being
// retyped per method.
package ticket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers three cases on purpose: no such ticket, a ticket in a workspace
// the caller is not a member of, and an archived ticket. A caller learns only that it
// cannot have this row - never that it exists, which a 403 would confirm.
//
// internal/document.Save made the same call for the same reason.
var ErrNotFound = errors.New("ticket: not found")

// callerCanReach is the scoping predicate, written once and composed into every
// statement that names a ticket.
//
// A ticket reaches its workspace through project and team, and the caller must be a
// member of that workspace. $ACCOUNT is a named placeholder rather than a number so
// the same text drops into statements whose other parameters differ without the
// numbering quietly going wrong - expand fills it in.
//
// An EXISTS rather than a join, so an UPDATE can carry the identical predicate a
// SELECT does. Three hand-written copies would be three chances to omit it.
const callerCanReach = `EXISTS (
	        SELECT 1 FROM project p
	          JOIN team tm ON tm.id = p.team_id
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE p.id = t.project_id)`

// liveOnly is the archive predicate, also written once. 0029 explains why it exists;
// this is where its cost is paid, so no caller pays it.
const liveOnly = `t.archived_at IS NULL`

/*
┏━ Ticket ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one ticket as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID           string
┃      ProjectID    string
┃      Title        string
┃      Description  string        never null, '' when unset
┃      StatusID     *string       null is normal, not an edge case
┃      AssigneeID   *string       null is normal, not an edge case
┃      CreatedAt    time.Time
┃      UpdatedAt    time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Get · Update         one per call
*/

type Ticket struct {
	ID          string
	ProjectID   string
	Title       string
	Description string
	StatusID    *string
	AssigneeID  *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of the ticket table
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create(ctx, accountID, CreateParams)   →  Ticket, error
┃      Get(ctx, accountID, id)                →  Ticket, error
┃      Update(ctx, accountID, UpdateParams)   →  Ticket, error
┃      Archive(ctx, accountID, id)            →  error
┃      Unarchive(ctx, accountID, id)          →  error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service owning the ticket table through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// CreateParams is a new ticket's writable state. A struct rather than positional
// arguments for the reason SaveParams gives: transposing two strings compiles cleanly
// and corrupts data.
type CreateParams struct {
	ProjectID   string
	Title       string
	Description string
	StatusID    *string
	AssigneeID  *string
}

// UpdateParams replaces a ticket's writable state wholesale.
//
// Deliberately a full replace rather than a patch. A patch needs every field to
// distinguish "leave alone" from "clear", which for a nullable column means a pointer
// to a pointer or a per-field presence flag - and both are the kind of API where a
// caller clears an assignee by accident. LAM-59's filters do not need patch semantics,
// and the first caller that does can add PATCH alongside this.
type UpdateParams struct {
	ID          string
	Title       string
	Description string
	StatusID    *string
	AssigneeID  *string
}

const columns = `t.id::text, t.project_id::text, t.title, t.description,
                 t.status_id::text, t.assignee_account_id::text,
                 t.created_at, t.updated_at`

/*
┌─ ticket ────────────────────────────────────────
│  inserts a ticket into a project the caller can reach
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string        the caller
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Ticket
│      error                      ErrNotFound for an unreachable project
├─ example ───────────────────────────────────────
│      "Fix login"  →  Ticket{ID: "8b1c…"}
*/

func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The insert is gated on the caller reaching the project, not on the project
	// existing. A project in someone else's workspace returns no row, so this
	// answers ErrNotFound rather than creating a ticket the caller cannot then read.
	var t Ticket
	err = tx.QueryRow(ctx,
		`INSERT INTO ticket (project_id, title, description, status_id, assignee_account_id)
		 SELECT p.id, $2, $3, $4::uuid, $5::uuid
		   FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $6::uuid
		  WHERE p.id = $1::uuid
		 RETURNING id::text, project_id::text, title, description,
		           status_id::text, assignee_account_id::text, created_at, updated_at`,
		p.ProjectID, p.Title, p.Description, p.StatusID, p.AssigneeID, accountID,
	).Scan(&t.ID, &t.ProjectID, &t.Title, &t.Description,
		&t.StatusID, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: insert: %w", err)
	}

	if err := search.IndexTicket(ctx, tx, t.ID); err != nil {
		return Ticket{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  one live ticket the caller may see
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      Ticket
│      error        ErrNotFound: absent, archived, or not yours
*/

func (s *Service) Get(ctx context.Context, accountID, id string) (Ticket, error) {
	var t Ticket
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM ticket t
		         WHERE t.id = $1::uuid AND `+liveOnly+` AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(&t.ID, &t.ProjectID, &t.Title, &t.Description,
		&t.StatusID, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: get: %w", err)
	}

	return t, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  replaces a live ticket's writable state
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            UpdateParams
├─ out ───────────────────────────────────────────
│      Ticket                     as written
│      error                      ErrNotFound
*/

func (s *Service) Update(ctx context.Context, accountID string, p UpdateParams) (Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// updated_at is set here, not by a trigger and not by the column default. The
	// default only fires on insert, so without this every updated_at in the table
	// would be a creation timestamp wearing a misleading name.
	var t Ticket
	err = tx.QueryRow(ctx,
		expand(`UPDATE ticket t
		    SET title = $2, description = $3, status_id = $4::uuid,
		        assignee_account_id = $5::uuid, updated_at = now()
		  WHERE t.id = $1::uuid AND `+liveOnly+` AND `+callerCanReach+`
		 RETURNING t.id::text, t.project_id::text, t.title, t.description,
		           t.status_id::text, t.assignee_account_id::text,
		           t.created_at, t.updated_at`, "$6"),
		p.ID, p.Title, p.Description, p.StatusID, p.AssigneeID, accountID,
	).Scan(&t.ID, &t.ProjectID, &t.Title, &t.Description,
		&t.StatusID, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: update: %w", err)
	}

	// Reindex rather than leave the old text: the title is denormalised into
	// search_index, so an un-reindexed edit leaves search returning the old title.
	if err := search.ClearTicket(ctx, tx, t.ID); err != nil {
		return Ticket{}, err
	}
	if err := search.IndexTicket(ctx, tx, t.ID); err != nil {
		return Ticket{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  archives a ticket and drops it out of search
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      error                      ErrNotFound if already archived
*/

// Archive is the Delete verb of the CRUD pattern, and 0029 is why it is not a DELETE.
// A hard delete CASCADEs to comment and takes the discussion with it.
func (s *Service) Archive(ctx context.Context, accountID, id string) error {
	return s.setArchived(ctx, accountID, id, true)
}

/*
┌─ ticket ────────────────────────────────────────
│  restores an archived ticket, and reindexes it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      error                      ErrNotFound if not archived
*/

// Unarchive exists because archiving without it is just a slower delete. It is the
// undo that justified choosing archive at all.
func (s *Service) Unarchive(ctx context.Context, accountID, id string) error {
	return s.setArchived(ctx, accountID, id, false)
}

func (s *Service) setArchived(ctx context.Context, accountID, id string, archived bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The predicate is the inverse of the target state, so both directions are
	// idempotent-by-refusal: archiving an archived ticket finds no row and answers
	// ErrNotFound rather than silently moving the timestamp.
	var value any
	if archived {
		value = time.Now()
	}

	var found string
	err = tx.QueryRow(ctx,
		expand(`UPDATE ticket t
		    SET archived_at = $2, updated_at = now()
		  WHERE t.id = $1::uuid
		    AND (t.archived_at IS NULL) = $3
		    AND `+callerCanReach+`
		 RETURNING t.id::text`, "$4"),
		id, value, archived, accountID,
	).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("ticket: set archived: %w", err)
	}

	// Archiving has to remove the index row, not merely set a column - otherwise an
	// archived ticket keeps answering searches. Unarchiving puts it back.
	if err := search.ClearTicket(ctx, tx, found); err != nil {
		return err
	}
	if !archived {
		if err := search.IndexTicket(ctx, tx, found); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ticket: commit: %w", err)
	}

	return nil
}

// expand fills callerCanReach's named placeholder with the statement's real parameter
// number.
func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

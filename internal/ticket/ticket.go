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
║      internal/api  →  create · get · update · archive · unarchive
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
┃      Version      int           1 on create, +1 per write
┃      UpdatedBy    *string       null: not recorded, or account gone
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

	// Version is the precondition token. It starts at 1 and rises by exactly 1 in
	// the same statement as every update, archive and unarchive, so it names one
	// state of the ticket and no other. 0031 explains why it is not updated_at.
	Version int

	// UpdatedBy is the account that wrote the current state. Null for a row from
	// before 0031 and for a writer whose account has since been deleted - work
	// outlives the people who touched it - so a caller renders null as "someone".
	UpdatedBy *string
}

// dest is the scan target for columns, in columns' order. Written once because the
// list is scanned in four places and a fifth column added to three of them compiles
// and fails at runtime.
func (t *Ticket) dest() []any {
	return []any{&t.ID, &t.ProjectID, &t.Title, &t.Description,
		&t.StatusID, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt, &t.Version, &t.UpdatedBy}
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
┃      Archive(ctx, accountID, id, ifVersion)    →  Ticket, error
┃      Unarchive(ctx, accountID, id, ifVersion)  →  Ticket, error
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

	// IfVersion is the version the caller last saw. Nil means no precondition, and
	// the write is last-write-wins exactly as it was before 0031 - an API-token
	// script has no version to send. Non-nil makes the write conditional: it
	// happens only if the ticket is still at that version, and otherwise the
	// answer is a *Conflict carrying what is there now.
	IfVersion *int
}

const columns = `t.id::text, t.project_id::text, t.title, t.description,
                 t.status_id::text, t.assignee_account_id::text,
                 t.created_at, t.updated_at, t.version, t.updated_by::text`

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
		`INSERT INTO ticket (project_id, title, description, status_id, assignee_account_id, updated_by)
		 SELECT p.id, $2, $3, $4::uuid, $5::uuid, $6::uuid
		   FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $6::uuid
		  WHERE p.id = $1::uuid
		 RETURNING id::text, project_id::text, title, description,
		           status_id::text, assignee_account_id::text, created_at, updated_at,
		           version, updated_by::text`,
		p.ProjectID, p.Title, p.Description, p.StatusID, p.AssigneeID, accountID,
	).Scan(t.dest()...)
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
	).Scan(t.dest()...)
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
│      p            UpdateParams    IfVersion nil = no precondition
├─ out ───────────────────────────────────────────
│      Ticket                     as written, Version + 1
│      error                      ErrNotFound · *Conflict
├─ example ───────────────────────────────────────
│      IfVersion 4, ticket at 5   →  *Conflict, Current.Version 5
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
	//
	// The version check is the last predicate of the same statement that writes, which
	// is the whole of the atomicity: there is no read before it for a second writer to
	// slip between. Two writers holding the same version both reach this UPDATE; the
	// second blocks on the first's row lock, re-evaluates the WHERE against the row the
	// first committed, finds the version moved, and matches nothing. Exactly one wins
	// without any explicit locking here, and without SERIALIZABLE.
	var t Ticket
	err = tx.QueryRow(ctx,
		expand(`UPDATE ticket t
		    SET title = $2, description = $3, status_id = $4::uuid,
		        assignee_account_id = $5::uuid, updated_at = now(),
		        updated_by = $6::uuid, version = t.version + 1
		  WHERE t.id = $1::uuid AND `+liveOnly+` AND `+callerCanReach+`
		    AND ($7::int IS NULL OR t.version = $7::int)
		 RETURNING `+columns, "$6"),
		p.ID, p.Title, p.Description, p.StatusID, p.AssigneeID, accountID, p.IfVersion,
	).Scan(t.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, s.noRow(ctx, tx, accountID, p.ID, p.IfVersion, false)
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
│      ifVersion    *int          nil = no precondition
├─ out ───────────────────────────────────────────
│      Ticket                     as archived, Version + 1
│      error                      ErrNotFound · *Conflict
*/

// Archive is the Delete verb of the CRUD pattern, and 0029 is why it is not a DELETE.
// A hard delete CASCADEs to comment and takes the discussion with it.
//
// It returns the ticket because the caller needs the new version: a client that
// archived at version 4 is holding a ticket that is now at 5, and has no other way to
// learn that short of a read - which, for an archived ticket, answers not found.
func (s *Service) Archive(ctx context.Context, accountID, id string, ifVersion *int) (Ticket, error) {
	return s.setArchived(ctx, accountID, id, true, ifVersion)
}

/*
┌─ ticket ────────────────────────────────────────
│  restores an archived ticket, and reindexes it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
│      ifVersion    *int          nil = no precondition
├─ out ───────────────────────────────────────────
│      Ticket                     as restored, Version + 1
│      error                      ErrNotFound · *Conflict
*/

// Unarchive exists because archiving without it is just a slower delete. It is the
// undo that justified choosing archive at all.
func (s *Service) Unarchive(ctx context.Context, accountID, id string, ifVersion *int) (Ticket, error) {
	return s.setArchived(ctx, accountID, id, false, ifVersion)
}

func (s *Service) setArchived(ctx context.Context, accountID, id string, archived bool, ifVersion *int) (Ticket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// The predicate is the inverse of the target state, so both directions are
	// idempotent-by-refusal: archiving an archived ticket finds no row and answers
	// ErrNotFound rather than silently moving the timestamp.
	//
	// now() rather than a timestamp from this process, so archived_at and updated_at
	// are the same instant and a client reading either says the same minute. The
	// archive and the unarchive are both writes, so both move version and updated_by;
	// archived_by is set on the way in and cleared on the way out, because a live
	// ticket has no archiver.
	var t Ticket
	err = tx.QueryRow(ctx,
		expand(`UPDATE ticket t
		    SET archived_at = CASE WHEN $2 THEN now() END,
		        archived_by = CASE WHEN $2 THEN $4::uuid END,
		        updated_at = now(), updated_by = $4::uuid, version = t.version + 1
		  WHERE t.id = $1::uuid
		    AND (t.archived_at IS NULL) = $2
		    AND ($3::int IS NULL OR t.version = $3::int)
		    AND `+callerCanReach+`
		 RETURNING `+columns, "$4"),
		id, archived, ifVersion, accountID,
	).Scan(t.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, s.noRow(ctx, tx, accountID, id, ifVersion, !archived)
	}
	if err != nil {
		return Ticket{}, fmt.Errorf("ticket: set archived: %w", err)
	}

	// Archiving has to remove the index row, not merely set a column - otherwise an
	// archived ticket keeps answering searches. Unarchiving puts it back.
	if err := search.ClearTicket(ctx, tx, t.ID); err != nil {
		return Ticket{}, err
	}
	if !archived {
		if err := search.IndexTicket(ctx, tx, t.ID); err != nil {
			return Ticket{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, nil
}

// expand fills callerCanReach's named placeholder with the statement's real parameter
// number.
func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

/*
╔═ aspect.go ═══════════════════════════════════════════════════════════════════════════
║  aspect · aspect types, and their fields as rows
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service            struct
║      Type               struct
║      Field              struct
║      ErrNotFound        error
║      ErrFieldSetWrong   error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  the aspect type editor's eight operations
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package aspect owns aspect_type and aspect_type_field.
//
// Every field edit here is a row operation - an INSERT, an UPDATE or a DELETE against
// aspect_type_field - and never a rewrite of anything. That is the whole reason
// aspect_type_field is a table rather than a JSON array on the aspect_type row: an
// array means every field edit rewrites a blob, and two people editing two different
// fields silently clobber each other.
//
// The rule has a second half that is easy to lose. document.body is keyed by
// aspect_type_field.id, so a field edit is *visible* in every document of that type
// without any document being touched - and this package must never touch one.
// internal/document owns that write path and sqlguard fails the build on any SQL here
// that names it. See the delete decision on RemoveField, where that guard stops being
// a formality and becomes the answer.
package aspect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound collapses absent and out-of-scope, the same call internal/ticket and
// internal/document make: a 403 would confirm the row exists, which a caller outside
// the workspace has not earned.
var ErrNotFound = errors.New("aspect: not found")

// ErrFieldSetWrong means a reorder named a set of fields that is not exactly the
// type's. See Reorder - a partial order is the one input here that can silently
// corrupt an ordering rather than fail.
var ErrFieldSetWrong = errors.New("aspect: reorder must name every field of the type, exactly once")

// callerCanReach is the scoping predicate for an aspect type, written once and
// composed into every statement that names one.
//
// Shorter than internal/ticket's by a hop: an aspect type hangs off a team directly,
// where a ticket reaches its team through a project. Same $ACCOUNT convention, and the
// same reason - a named placeholder survives being dropped into statements whose other
// parameters differ.
const callerCanReach = `EXISTS (
	        SELECT 1 FROM team tm
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE tm.id = at.team_id)`

/*
┏━ Type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one aspect type, and the fields it defines
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID           string
┃      TeamID       string
┃      Name         string
┃      Fields       []Field     ordered by position
┃      CreatedAt    time.Time
┃      UpdatedAt    time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      CreateType · Get · ListTypes · RenameType
*/

type Type struct {
	ID        string
	TeamID    string
	Name      string
	Fields    []Field
	CreatedAt time.Time
	UpdatedAt time.Time
}

/*
┏━ Field ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one field of an aspect type
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID              string      never changes, ever
┃      AspectTypeID    string
┃      Label           string      free to rename
┃      Position        int         a hint, not a slot
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      AddField · RenameField · Reorder · Get
*/

// Field.ID is load-bearing outside this package and must never be reassigned.
// document.body is a map keyed by it, so a new id orphans every value already stored
// under the old one - in every document of the type, unrecoverably and with nothing
// erroring. 0015 chose a uuid over a slug for exactly this reason, and
// internal/document's TestRenamingAFieldLabelLeavesDocumentsAlone pins the seam.
//
// Nothing in this package writes id. RenameField writes label.
type Field struct {
	ID           string
	AspectTypeID string
	Label        string
	Position     int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of aspect_type and aspect_type_field
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      CreateType(ctx, accountID, teamID, name)      →  Type, error
┃      ListTypes(ctx, accountID, teamID)             →  []Type, error
┃      Get(ctx, accountID, typeID)                   →  Type, error
┃      RenameType(ctx, accountID, typeID, name)      →  Type, error
┃      AddField(ctx, accountID, typeID, label)       →  Field, error
┃      RenameField(ctx, accountID, fieldID, label)   →  Field, error
┃      RemoveField(ctx, accountID, fieldID)          →  error
┃      Reorder(ctx, accountID, typeID, ids)          →  []Field, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service owning both aspect tables through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

const typeColumns = `at.id::text, at.team_id::text, at.name, at.created_at, at.updated_at`

const fieldColumns = `f.id::text, f.aspect_type_id::text, f.label, f.position,
                      f.created_at, f.updated_at`

/*
┌─ aspect ────────────────────────────────────────
│  creates an aspect type on a team the caller can reach
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      teamID       string
│      name         string
├─ out ───────────────────────────────────────────
│      Type                       with no fields yet
│      error                      ErrNotFound for an unreachable team
*/

// CreateType makes no fields. An aspect type with none is legal and renders as a
// document with a title and nothing else, which is what the editor starts from.
func (s *Service) CreateType(ctx context.Context, accountID, teamID, name string) (Type, error) {
	var t Type

	// Gated on the caller reaching the team, not on the team existing. A team in
	// someone else's workspace returns no row, so this answers ErrNotFound rather
	// than creating a type the caller cannot then read.
	err := s.pool.QueryRow(ctx,
		`INSERT INTO aspect_type (team_id, name)
		 SELECT tm.id, $2
		   FROM team tm
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $3::uuid
		  WHERE tm.id = $1::uuid
		 RETURNING id::text, team_id::text, name, created_at, updated_at`,
		teamID, name, accountID,
	).Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Type{}, ErrNotFound
	}
	if err != nil {
		return Type{}, fmt.Errorf("aspect: create type: %w", err)
	}

	return t, nil
}

/*
┌─ aspect ────────────────────────────────────────
│  every aspect type on one team
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      teamID       string
├─ out ───────────────────────────────────────────
│      []Type                     without fields
│      error
*/

// ListTypes deliberately leaves Fields empty. The editor's list is a sidebar of names,
// and loading every field of every type to draw it is a join nobody asked for - Get
// fills them in for the one type being edited.
func (s *Service) ListTypes(ctx context.Context, accountID, teamID string) ([]Type, error) {
	rows, err := s.pool.Query(ctx,
		expand(`SELECT `+typeColumns+` FROM aspect_type at
		         WHERE at.team_id = $1::uuid AND `+callerCanReach+`
		         ORDER BY at.name, at.id`, "$2"),
		teamID, accountID)
	if err != nil {
		return nil, fmt.Errorf("aspect: list types: %w", err)
	}
	defer rows.Close()

	var out []Type
	for rows.Next() {
		var t Type
		if err := rows.Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("aspect: list types scan: %w", err)
		}
		out = append(out, t)
	}

	return out, rows.Err()
}

/*
┌─ aspect ────────────────────────────────────────
│  one aspect type with its fields, in order
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      typeID       string
├─ out ───────────────────────────────────────────
│      Type
│      error                      ErrNotFound
*/

func (s *Service) Get(ctx context.Context, accountID, typeID string) (Type, error) {
	var t Type

	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+typeColumns+` FROM aspect_type at
		         WHERE at.id = $1::uuid AND `+callerCanReach, "$2"),
		typeID, accountID,
	).Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Type{}, ErrNotFound
	}
	if err != nil {
		return Type{}, fmt.Errorf("aspect: get type: %w", err)
	}

	t.Fields, err = s.fields(ctx, s.pool, typeID)
	if err != nil {
		return Type{}, err
	}

	return t, nil
}

/*
┌─ aspect ────────────────────────────────────────
│  renames an aspect type
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      typeID       string
│      name         string
├─ out ───────────────────────────────────────────
│      Type                       with its fields
│      error                      ErrNotFound
*/

func (s *Service) RenameType(ctx context.Context, accountID, typeID, name string) (Type, error) {
	var t Type

	err := s.pool.QueryRow(ctx,
		expand(`UPDATE aspect_type at SET name = $2, updated_at = now()
		         WHERE at.id = $1::uuid AND `+callerCanReach+`
		     RETURNING at.id::text, at.team_id::text, at.name, at.created_at, at.updated_at`, "$3"),
		typeID, name, accountID,
	).Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Type{}, ErrNotFound
	}
	if err != nil {
		return Type{}, fmt.Errorf("aspect: rename type: %w", err)
	}

	t.Fields, err = s.fields(ctx, s.pool, typeID)
	if err != nil {
		return Type{}, err
	}

	return t, nil
}

/*
┌─ aspect ────────────────────────────────────────
│  appends a field to an aspect type
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      typeID       string
│      label        string
├─ out ───────────────────────────────────────────
│      Field                      at the end
│      error                      ErrNotFound
├─ example ───────────────────────────────────────
│      "Methods"  →  Field{Position: 3}
*/

// AddField is an INSERT and nothing else. No document is read, written, or even
// named: a new field appears in every document of this type as a key that is simply
// absent from body, which renders as empty. That is the property the table buys.
func (s *Service) AddField(ctx context.Context, accountID, typeID, label string) (Field, error) {
	var f Field

	// The position is computed in the statement rather than read first and written
	// back, so two concurrent adds cannot both pick the same number by racing
	// between a SELECT and an INSERT. They can still land on the same position if
	// they interleave inside one another, which is fine - 0015 declines a unique
	// index on (aspect_type_id, position) on purpose, and position is an ordering
	// hint rather than a slot.
	err := s.pool.QueryRow(ctx,
		expand(`INSERT INTO aspect_type_field (aspect_type_id, label, position)
		        SELECT at.id, $2,
		               coalesce((SELECT max(x.position) FROM aspect_type_field x
		                          WHERE x.aspect_type_id = at.id), 0) + 1
		          FROM aspect_type at
		         WHERE at.id = $1::uuid AND `+callerCanReach+`
		     RETURNING id::text, aspect_type_id::text, label, position,
		               created_at, updated_at`, "$3"),
		typeID, label, accountID,
	).Scan(&f.ID, &f.AspectTypeID, &f.Label, &f.Position, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Field{}, ErrNotFound
	}
	if err != nil {
		return Field{}, fmt.Errorf("aspect: add field: %w", err)
	}

	return f, nil
}

/*
┌─ aspect ────────────────────────────────────────
│  renames a field, leaving its id alone
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      fieldID      string
│      label        string
├─ out ───────────────────────────────────────────
│      Field
│      error                      ErrNotFound
*/

// RenameField writes label and nothing else. Every document of this type keeps its
// values, because they are keyed by id - which is the seam
// TestRenamingAFieldLabelLeavesDocumentsAlone exists to hold.
func (s *Service) RenameField(ctx context.Context, accountID, fieldID, label string) (Field, error) {
	var f Field

	err := s.pool.QueryRow(ctx,
		expand(`UPDATE aspect_type_field f SET label = $2, updated_at = now()
		          FROM aspect_type at
		         WHERE f.id = $1::uuid AND at.id = f.aspect_type_id AND `+callerCanReach+`
		     RETURNING `+fieldColumns, "$3"),
		fieldID, label, accountID,
	).Scan(&f.ID, &f.AspectTypeID, &f.Label, &f.Position, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Field{}, ErrNotFound
	}
	if err != nil {
		return Field{}, fmt.Errorf("aspect: rename field: %w", err)
	}

	return f, nil
}

/*
┌─ aspect ────────────────────────────────────────
│  removes a field, and leaves documents untouched
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      fieldID      string
├─ out ───────────────────────────────────────────
│      error                      ErrNotFound
*/

// RemoveField deletes the row and stops there. LAM-44 asked for this to be decided
// deliberately, so here is the decision and its reason.
//
// A deleted field's key stays in document.body, in every document of the type. It is
// no longer described by any field, so nothing renders it - but the value is still
// there, and re-adding a field would not bring it back (a new field is a new uuid), so
// this is a hiding rather than an undo.
//
// Stripping the keys instead was rejected on two grounds. It is unrecoverable: the
// values are gone the moment the statement commits, and a misclick in an editor is not
// a good reason to lose a column of data across every document of a type. And it would
// mean rewriting every one of those bodies - the blob rewrite this table exists to
// avoid - from a package that sqlguard forbids from naming document at all. The guard
// is not incidental here; it is the shape of the answer.
//
// What this leaves behind is orphaned keys accumulating in body over time. That is
// real, and it is a reporting problem rather than a correctness one: a sweep that
// lists them is a thing someone can write later, against data that still exists. The
// reverse is not available.
func (s *Service) RemoveField(ctx context.Context, accountID, fieldID string) error {
	var gone string

	err := s.pool.QueryRow(ctx,
		expand(`DELETE FROM aspect_type_field f
		         USING aspect_type at
		         WHERE f.id = $1::uuid AND at.id = f.aspect_type_id AND `+callerCanReach+`
		     RETURNING f.id::text`, "$2"),
		fieldID, accountID,
	).Scan(&gone)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("aspect: remove field: %w", err)
	}

	return nil
}

/*
┌─ aspect ────────────────────────────────────────
│  rewrites every position in one statement
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      typeID       string
│      fieldIDs     []string      the whole set, in order
├─ out ───────────────────────────────────────────
│      []Field                    as written
│      error        ErrNotFound · ErrFieldSetWrong
├─ example ───────────────────────────────────────
│      [c, a, b]  →  positions 1, 2, 3
*/

// Reorder takes the complete ordered set, not a move instruction, and that is the
// decision worth defending.
//
// A "move field X to position 3" API has to decide what happens to everything between
// the old and new position, and two clients issuing overlapping moves produce an order
// neither asked for. Sending the whole list makes the request idempotent and makes
// the result exactly what the client drew.
//
// It costs one check: the list has to be exactly this type's fields, each once. A
// partial list would renumber some rows and leave others, which is the one input here
// that corrupts an ordering rather than failing - so ErrFieldSetWrong exists and the
// check runs inside the transaction that does the writing.
//
// The write itself is a single UPDATE ... FROM unnest(...) WITH ORDINALITY. No
// temporary values, no two-phase swap: 0015 declines UNIQUE (aspect_type_id, position)
// precisely so that a reorder is one plain statement, and this is that statement.
func (s *Service) Reorder(ctx context.Context, accountID, typeID string, fieldIDs []string) ([]Field, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("aspect: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Scope first and on its own: without this a caller outside the workspace would
	// reach the set check below and learn how many fields the type has.
	var reachable string
	err = tx.QueryRow(ctx,
		expand(`SELECT at.id::text FROM aspect_type at
		         WHERE at.id = $1::uuid AND `+callerCanReach, "$2"),
		typeID, accountID).Scan(&reachable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("aspect: reorder scope: %w", err)
	}

	current, err := s.fields(ctx, tx, typeID)
	if err != nil {
		return nil, err
	}
	if !sameSet(current, fieldIDs) {
		return nil, ErrFieldSetWrong
	}

	// WITH ORDINALITY numbers the array in the order it was sent, so the position
	// column is written straight from the client's ordering in one pass.
	if _, err := tx.Exec(ctx,
		`UPDATE aspect_type_field f
		    SET position = o.ord, updated_at = now()
		   FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, ord)
		  WHERE f.id = o.id AND f.aspect_type_id = $1::uuid`,
		typeID, fieldIDs); err != nil {
		return nil, fmt.Errorf("aspect: reorder: %w", err)
	}

	out, err := s.fields(ctx, tx, typeID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("aspect: commit: %w", err)
	}

	return out, nil
}

// querier is whatever can run a query - the pool, or a transaction. fields is read
// both ways: on its own after a Get, and inside Reorder's transaction where it has to
// see that transaction's writes.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// fields reads one type's fields in order. Scoping is the caller's job here - every
// exported method has already proved the type is reachable before calling this, and
// repeating the predicate would be a second place to get it wrong.
func (s *Service) fields(ctx context.Context, q querier, typeID string) ([]Field, error) {
	rows, err := q.Query(ctx,
		`SELECT `+fieldColumns+` FROM aspect_type_field f
		  WHERE f.aspect_type_id = $1::uuid
		  ORDER BY f.position, f.id`, typeID)
	if err != nil {
		return nil, fmt.Errorf("aspect: fields: %w", err)
	}
	defer rows.Close()

	var out []Field
	for rows.Next() {
		var f Field
		if err := rows.Scan(&f.ID, &f.AspectTypeID, &f.Label, &f.Position,
			&f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("aspect: fields scan: %w", err)
		}
		out = append(out, f)
	}

	return out, rows.Err()
}

// sameSet reports whether ids names every field in current exactly once. A duplicate
// is as wrong as an omission: it would leave one field renumbered twice and another
// not at all.
func sameSet(current []Field, ids []string) bool {
	if len(current) != len(ids) {
		return false
	}

	want := make(map[string]bool, len(current))
	for _, f := range current {
		want[f.ID] = true
	}

	for _, id := range ids {
		if !want[id] {
			return false
		}
		delete(want, id)
	}

	return len(want) == 0
}

// expand fills callerCanReach's named placeholder with the statement's real parameter
// number, the same way internal/ticket does.
func expand(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

/*
┌─ aspect ────────────────────────────────────────
│  seeds a brand-new team's starter aspect types
├─ in ────────────────────────────────────────────
│      ctx       context.Context
│      tx        pgx.Tx        the team's creation
│      teamID    string
│      names     []string
├─ out ───────────────────────────────────────────
│      []Type
│      error
├─ example ───────────────────────────────────────
│      [Class, Service]  →  two rows, no scoping
*/

// Bootstrap is the one entry point here that does not check the caller, and the
// signature is what makes that safe rather than a comment asking nicely.
//
// It takes a pgx.Tx, which only the package that opened it holds, and it is called from
// inside the transaction that is creating the team - where the team does not exist yet,
// so there is no membership to check and no row anyone else could reach. Routing this
// through CreateType instead would mean the scoping predicate joining workspace_member
// against a team created moments ago in the same uncommitted transaction, which is a
// check that proves nothing and can only fail for the wrong reasons.
//
// LAM-43's reason for running inside that transaction: a team must never be observable
// half-configured. If the seeding fails, the team was never created either.
func Bootstrap(ctx context.Context, tx pgx.Tx, teamID string, names []string) ([]Type, error) {
	out := make([]Type, 0, len(names))

	for _, name := range names {
		var t Type
		err := tx.QueryRow(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, $2)
			 RETURNING id::text, team_id::text, name, created_at, updated_at`,
			teamID, name,
		).Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("aspect: bootstrap %q: %w", name, err)
		}

		out = append(out, t)
	}

	return out, nil
}

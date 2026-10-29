// Package document owns a document's body blob. Service.Save is the only code path
// permitted to write one.
//
// It no longer owns search_index. That moved to internal/search under LAM-45, when
// tickets and comments became sources too and the index stopped being any one table's
// derived data. Save still keeps the blob and its rows in step, in one transaction -
// the ownership moved, the invariant did not.
//
// boundary_test.go enforces the rule against the rest of the module;
// docs/adr/0001-write-path-enforcement.md says why.
package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/activity"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers three cases on purpose: no such document, a document in a workspace
// the caller is not a member of, and an archived document that the call may not see. A
// caller learns only that it cannot have this row - never that it exists.
var ErrNotFound = errors.New("document not found")

// ErrAspectTypeNotFound means the aspect type a write named is absent or belongs to
// another workspace - one answer for both, the collapse internal/aspect makes.
var ErrAspectTypeNotFound = errors.New("document: aspect type not found")

// The reasons a write is refused as invalid. Each is the Kind of a *FieldError, which says
// where; the API maps the pair to a 422 naming the field.
var (
	// ErrUnknownField: a body key that is not an aspect_type_field id of the document's
	// type. FieldError.Field is that key.
	ErrUnknownField = errors.New("document: body key is not a field of the document's type")

	// ErrNoFields: a normal document cannot become an aspect of a type that has no fields,
	// because its body would have nowhere to go. Field is aspect_type_id.
	ErrNoFields = errors.New("document: the aspect type has no fields to move the body into")

	// ErrTypeShape: aspect_type_id missing on an aspect document, or present on a normal
	// one. The database enforces the same biconditional (document_aspect_type_matches_type);
	// checking it here turns a constraint violation into a 422 that names the field.
	ErrTypeShape = errors.New("document: aspect_type_id must be set exactly when type is aspect")

	// ErrScopeNotFound: team_id or project_id names nothing the caller can use. One cause
	// for absent and for another workspace's, so it cannot be used to probe for ids.
	ErrScopeNotFound = errors.New("document: scope not found")

	// ErrScopeAmbiguous: both team_id and project_id. The database allows at most one
	// (document_at_most_one_scope).
	ErrScopeAmbiguous = errors.New("document: at most one of team_id and project_id")
)

// FieldError is a write refused as invalid, and which input it was about.
type FieldError struct {
	// Kind is one of the Err* values above.
	Kind error
	// Field is the request member, or for ErrUnknownField the offending body key.
	Field string
}

func (e *FieldError) Error() string { return fmt.Sprintf("%v (%s)", e.Kind, e.Field) }
func (e *FieldError) Unwrap() error { return e.Kind }

// MaxTitleLength is the longest title a write accepts, in characters (internal/textlimit).
// Enforced here and not as a CHECK, for the reason ticket's limits are: rows written before
// the limit existed must stay writable.
const MaxTitleLength = 200

// ContentKey is the one body key a normal document uses: its text, as Markdown. A normal
// document has no aspect type and so no field ids, and this is what the "body moves into
// the type's first field" rule moves. docs/adr/0006 records the choice.
const ContentKey = "content"

// Service is the only type permitted to write document bodies. Handlers depend on
// this; they never reach for the pool themselves.
type Service struct {
	pool *pgxpool.Pool

	// aspects answers "which body keys does this type allow". internal/aspect owns the
	// tables, document may not read them (aspect/boundary_test.go), so it asks. Built
	// from the same pool: aspect.Service holds nothing else.
	aspects *aspect.Service

	// reactions run inside an update's transaction; see OnWrite.
	reactions []Reaction
}

// Change is what an update did to a document's body, handed to each Reaction.
type Change struct {
	DocumentID  string
	WorkspaceID string
	Before      map[string]any
	After       map[string]any

	// Conversion is set only when the write turned a normal document into an aspect one
	// (LAM-136), and nil otherwise. Anchors that named ContentKey must follow the text.
	Conversion *Conversion
}

// Conversion says where a normal document's text went when it became an aspect document.
type Conversion struct {
	// Fields are the new aspect type's field ids, in the type's order.
	Fields []string
	// MovedTo is the field id that received the ContentKey text unchanged, or empty when
	// no text moved: the body had none, or the request filled the first field itself and so
	// the old text was dropped.
	MovedTo string
}

// Reaction is code that must move in step with a document's body but owns tables this package
// may not write (LAM-122: internal/comment, which owns review requests and itself imports this
// package, so it cannot be called directly). It runs in the update's transaction, after the
// body is written and before the commit, so its writes and the body commit or roll back
// together. It returns the ids of the review requests it sent back to pending, which the
// caller announces after the commit (Document.StaledReviews). It also returns the ones it re-anchored when Change.Conversion is set (LAM-136).
type Reaction func(ctx context.Context, tx pgx.Tx, c Change) ([]string, error)

// OnWrite registers a Reaction. Call it while wiring the service, before it serves requests;
// it is not safe to call concurrently with writes. Only updates react: a create has no
// earlier text for anything to be stale against.
func (s *Service) OnWrite(r Reaction) { s.reactions = append(s.reactions, r) }

// NewService returns a Service backed by pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, aspects: aspect.NewService(pool)}
}

// Document type values. The database enforces this same closed set in
// document_type_is_known; these exist so callers do not spell it themselves.
const (
	TypeNormal = "normal"
	TypeAspect = "aspect"
)

// Document is one row as a caller sees it.
type Document struct {
	ID          string
	WorkspaceID string
	Title       string
	Type        string
	// AspectTypeID, TeamID and ProjectID are null when unset. A document with neither team
	// nor project is a workspace-level document.
	AspectTypeID *string
	TeamID       *string
	ProjectID    *string
	Body         map[string]any

	// Version starts at 1 and rises by exactly 1 on every update, archive and unarchive,
	// in the statement that writes. See 0035.
	Version    int
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ArchivedAt *time.Time

	// StaledReviews is set only on the Document an update returns: the ids of approved
	// review requests that update sent back to pending because the text they were about
	// changed materially (LAM-122). The caller announces them after the commit.
	StaledReviews []string

	// Activity is set only on the Document a write returns: the log entries that write made
	// (LAM-89), for the caller to announce after the commit. Reads leave it nil.
	Activity []activity.Entry
}

// columns and dest are written once because the row is scanned from four statements.
const columns = `d.id::text, d.workspace_id::text, d.title, d.type, d.aspect_type_id::text,
	d.team_id::text, d.project_id::text, d.body::text, d.version, d.created_at, d.updated_at, d.archived_at`

func (d *Document) scan(row pgx.Row) error {
	var raw string
	if err := row.Scan(&d.ID, &d.WorkspaceID, &d.Title, &d.Type, &d.AspectTypeID,
		&d.TeamID, &d.ProjectID, &raw, &d.Version, &d.CreatedAt, &d.UpdatedAt, &d.ArchivedAt); err != nil {
		return err
	}

	return d.decode(raw)
}

func (d *Document) decode(raw string) error {
	d.Body = map[string]any{}
	if err := json.Unmarshal([]byte(raw), &d.Body); err != nil {
		return fmt.Errorf("decode body %s: %w", d.ID, err)
	}

	return nil
}

// SaveParams is one document's writable state.
//
// It is a struct rather than a parameter list because LAM-23 gave document
// five writable columns, and five positional strings is a signature where
// transposing two of them compiles cleanly and corrupts data.
//
// Zero values are meaningful and match the column defaults: an empty ID
// inserts, an empty Title is an untitled document, an empty Type is
// TypeNormal, and an empty AspectTypeID is SQL NULL.
type SaveParams struct {
	// WorkspaceID scopes the write. Either this or AccountID is required; with both, the
	// write is scoped to this workspace and the account must belong to it.
	WorkspaceID string
	// AccountID is the caller, for the API. It scopes the write to the workspaces the
	// account is a member of, and on an insert with no WorkspaceID, team or project names
	// the workspace: the account's oldest membership (ADR 0004).
	AccountID string
	// ID selects an existing document. Empty inserts a new one.
	ID string
	// Title may be empty; the column is NOT NULL DEFAULT ''.
	Title string
	// Type is TypeNormal or TypeAspect. Empty means TypeNormal.
	Type string
	// AspectTypeID must be set when Type is TypeAspect and empty otherwise.
	// The database enforces both directions in
	// document_aspect_type_matches_type.
	AspectTypeID string
	// ProjectID and TeamID file a new document under one of them; at most one. On an insert
	// they are read as given. On an update TeamID is ignored and ProjectID is read only when
	// MoveProject is set (LAM-138): an update that does not ask leaves the scope alone.
	ProjectID string
	TeamID    string
	// MoveProject, on an update, moves the document to ProjectID, or - when ProjectID is
	// empty - takes it out of its project (a workspace-level, "team-wide" document). Filing
	// under a project clears a team_id, as the exclusive arc requires; taking it out of a
	// project leaves team_id as it was. A project the caller cannot reach is
	// ErrScopeNotFound at project_id. Ignored on an insert.
	MoveProject bool
	// Body is the field map. Keys are aspect_type_field IDs; for an aspect document each
	// one must be a field of AspectTypeID (ErrUnknownField). A normal document's body is
	// not checked, and by convention holds ContentKey.
	Body map[string]any
	// IfVersion makes an update conditional on the document still being at that version.
	// Nil means unconditional, the rule internal/ticket.Update has. Ignored on an insert.
	IfVersion *int
	// IdempotencyKey, when set on an insert, makes the create safe to retry: the same key
	// from the same AccountID returns the document the first call made. It needs an
	// AccountID (a key is scoped to its creator) and is ignored on an update. See
	// WriteIdempotent. Nil is an ordinary create.
	IdempotencyKey *string
}

// normalize fills in the defaults the columns would apply, so the same values
// reach an INSERT and an UPDATE. Without this an update would blank Type on
// every caller that did not set it.
func (p SaveParams) normalize() SaveParams {
	if p.Type == "" {
		p.Type = TypeNormal
	}
	return p
}

// aspectType returns AspectTypeID as a value pgx will write as SQL NULL when
// it is unset, since an empty string is not a valid uuid.
func (p SaveParams) aspectType() *string {
	return nilIfEmpty(p.AspectTypeID)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Save writes a document and regenerates that document's search_index rows in
// one transaction. An empty p.ID inserts a new document; otherwise the existing
// document is replaced. It returns the document's ID.
//
// Save is Write for the callers that want only the id. They are the same write.
func (s *Service) Save(ctx context.Context, p SaveParams) (string, error) {
	d, err := s.Write(ctx, p)
	if err != nil {
		return "", err
	}

	return d.ID, nil
}

// Write is Save returning the row as written - its version above all, which the caller
// needs for the ETag and cannot get from a read without racing the next writer.
//
// Every write is scoped, to p.WorkspaceID or to the workspaces p.AccountID belongs to.
// Updating a document outside that scope returns ErrNotFound rather than a distinct
// error, so a caller outside the owning workspace learns nothing about whether it exists.
//
// This is the single write path required by LAM-3: the blob and the index move
// together or not at all. LAM-23 widened it from the body alone to the whole
// row, so that "the service owns document writes" stays true of the columns
// added around the blob rather than only of the blob. LAM-45 moved the index
// half to internal/search without loosening either property. LAM-87 added the scope,
// the version and the checks of an aspect document's body against its type.
//
// On an update, a document that is archived is not found, unless IfVersion is set, in
// which case it is a *Conflict with Archived set - the answer internal/ticket gives, for
// the same reason: a client holding unsynced edits needs to hear "gone", not "never was".
func (s *Service) Write(ctx context.Context, p SaveParams) (Document, error) {
	d, _, err := s.WriteIdempotent(ctx, p)

	return d, err
}

// WriteIdempotent is Write that also says whether the key had been used (LAM-126). When it
// had, nothing was written - no row, no version bump, no reindex - and the document is the
// original as it is now. An original archived since is a *Conflict with Replay set, so a
// client that lost the create's response learns the document exists instead of making
// another. The key is looked up before the write's own checks, so a replay returns what the
// first call made even if its team, project or aspect type has gone since; the INSERT's
// ON CONFLICT is what makes two concurrent requests with one key write exactly once.
func (s *Service) WriteIdempotent(ctx context.Context, p SaveParams) (Document, bool, error) {
	p = p.normalize()

	if p.WorkspaceID == "" && p.AccountID == "" {
		return Document{}, false, errors.New("document: a write needs a WorkspaceID or an AccountID")
	}
	if p.ID == "" && p.IdempotencyKey != nil && p.AccountID == "" {
		return Document{}, false, errors.New("document: an IdempotencyKey needs an AccountID")
	}

	if err := textlimit.Optional("title", p.Title, MaxTitleLength); err != nil {
		return Document{}, false, err
	}
	if err := p.checkShape(); err != nil {
		return Document{}, false, err
	}

	// Round-trip the body through JSON before extracting text. The rebuild
	// reads values back out of Postgres as decoded JSON, so normalizing here
	// guarantees both paths extract from identical Go values - an int 42 from a
	// handler and a float64 42 from the database cannot diverge.
	raw, err := json.Marshal(p.Body)
	if err != nil {
		return Document{}, false, fmt.Errorf("marshal body: %w", err)
	}

	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return Document{}, false, fmt.Errorf("normalize body: %w", err)
	}
	if normalized == nil {
		normalized = map[string]any{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Document{}, false, fmt.Errorf("begin: %w", err)
	}
	// Safe to call unconditionally - a rollback after a successful commit is a
	// no-op. This guarantees no early return can leak an open transaction.
	defer tx.Rollback(ctx)

	var d Document
	if p.ID == "" {
		var replayed bool
		d, replayed, err = s.insert(ctx, tx, p, normalized)
		if err != nil {
			return Document{}, false, err
		}
		if replayed {
			// Nothing was written, so nothing is reindexed: the original create's rows
			// are still there. The deferred rollback ends the transaction.
			return d, true, nil
		}
	} else {
		var (
			before map[string]any
			conv   *Conversion
		)
		d, before, conv, err = s.update(ctx, tx, p, normalized)
		if err != nil {
			return Document{}, false, err
		}

		for _, react := range s.reactions {
			staled, err := react(ctx, tx, Change{DocumentID: d.ID, WorkspaceID: d.WorkspaceID, Before: before, After: d.Body, Conversion: conv})
			if err != nil {
				return Document{}, false, err
			}
			d.StaledReviews = append(d.StaledReviews, staled...)
		}
	}

	// Both index calls take this transaction, so the blob and its rows still move
	// together or not at all. That invariant is the point of Save and survived the
	// move to internal/search unchanged.
	if err := search.ClearDocument(ctx, tx, d.ID); err != nil {
		return Document{}, false, err
	}

	if err := search.IndexDocument(ctx, tx, d.ID, d.Body); err != nil {
		return Document{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Document{}, false, fmt.Errorf("commit: %w", err)
	}

	return d, false, nil
}

// checkShape is the part of validation that needs no database.
func (p SaveParams) checkShape() error {
	if (p.Type == TypeAspect) != (p.AspectTypeID != "") {
		return &FieldError{Kind: ErrTypeShape, Field: "aspect_type_id"}
	}
	if p.ProjectID != "" && p.TeamID != "" {
		return &FieldError{Kind: ErrScopeAmbiguous, Field: "team_id"}
	}

	return nil
}

// checkBody is the rule that a body key must be a field of the document's type, and the
// move of a normal document's text into the type's first field when it becomes an aspect.
// It returns the body to store.
//
// current is the stored body of the document being converted, nil unless the write turns
// a normal document into an aspect one. The text moves from the request's ContentKey if
// the request carries one - that is the newer copy - and otherwise from the stored body,
// into the type's first field unless the request already filled it. Whatever else the
// normal body held is not carried: it is not a field id, and keeping it would store a key
// the next write would refuse.
func (s *Service) checkBody(ctx context.Context, workspaceID string, p SaveParams, body, current map[string]any, converting bool) (map[string]any, *Conversion, error) {
	if p.Type != TypeAspect {
		return body, nil, nil
	}

	ids, err := s.aspects.FieldIDs(ctx, workspaceID, p.AspectTypeID)
	if errors.Is(err, aspect.ErrNotFound) {
		return nil, nil, ErrAspectTypeNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("document: read aspect fields: %w", err)
	}

	var conv *Conversion
	if converting {
		conv = &Conversion{Fields: ids}
		if len(ids) == 0 {
			return nil, nil, &FieldError{Kind: ErrNoFields, Field: "aspect_type_id"}
		}

		content, ok := body[ContentKey]
		if ok {
			delete(body, ContentKey)
		} else {
			content, ok = current[ContentKey]
		}
		if _, filled := body[ids[0]]; ok && !filled {
			body[ids[0]] = content
			conv.MovedTo = ids[0]
		}
	}

	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	// Sorted, so the key named in the 422 is the same one on every run.
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !allowed[key] {
			return nil, nil, &FieldError{Kind: ErrUnknownField, Field: key}
		}
	}

	return body, conv, nil
}

// insert makes the row. The workspace comes from, in order: the team or project it is filed
// under, WorkspaceID, the caller's oldest membership. The bool is true for a replay of an
// Idempotency-Key, in which case nothing was written.
func (s *Service) insert(ctx context.Context, tx pgx.Tx, p SaveParams, body map[string]any) (Document, bool, error) {
	// A used key answers first, before the scope and body are checked against today's
	// state: a retry of a create that succeeded must not turn into a 404 because the
	// team was deleted since. The ON CONFLICT below still guards the race this read cannot.
	if p.IdempotencyKey != nil {
		if d, found, err := s.byKey(ctx, tx, p); err != nil || found {
			return d, found, err
		}
	}

	workspaceID, err := s.insertWorkspace(ctx, tx, p)
	if err != nil {
		return Document{}, false, err
	}

	body, _, err = s.checkBody(ctx, workspaceID, p, body, nil, false)
	if err != nil {
		return Document{}, false, err
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Document{}, false, fmt.Errorf("marshal body: %w", err)
	}

	var d Document
	err = d.scan(tx.QueryRow(ctx,
		`WITH d AS (
		     INSERT INTO document (workspace_id, body, title, type, aspect_type_id, project_id, team_id,
		                           created_by, idempotency_key)
		     VALUES ($1::uuid, $2::jsonb, $3, $4, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $9::text)
		     ON CONFLICT (created_by, idempotency_key) WHERE idempotency_key IS NOT NULL
		     DO NOTHING
		     RETURNING *)
		 SELECT `+columns+` FROM d`,
		workspaceID, string(raw), p.Title, p.Type, p.aspectType(),
		nilIfEmpty(p.ProjectID), nilIfEmpty(p.TeamID), nilIfEmpty(p.AccountID), p.IdempotencyKey,
	))
	if errors.Is(err, pgx.ErrNoRows) && p.IdempotencyKey != nil {
		// Lost the race to a concurrent create with the same key: its row is committed
		// now, and this new statement sees it.
		d, found, err := s.byKey(ctx, tx, p)
		if err == nil && !found {
			err = ErrNotFound
		}

		return d, found, err
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("write body: %w", err)
	}

	if err := d.note(ctx, tx, p.AccountID, activity.Created, nil, nil, nil); err != nil {
		return Document{}, false, err
	}

	return d, false, nil
}

// byKey reads the document p.AccountID created under p.IdempotencyKey. Scoped by the
// creator and by membership, so it finds only this account's own document and only while
// the account can still reach its workspace. Archived is read on purpose - whether the
// document is archived is half of the answer - so it is a *Conflict with Replay set.
func (s *Service) byKey(ctx context.Context, tx pgx.Tx, p SaveParams) (Document, bool, error) {
	var d Document
	err := d.scan(tx.QueryRow(ctx,
		`SELECT `+columns+` FROM document d
		  WHERE d.created_by = $1::uuid AND d.idempotency_key = $2::text AND `+fmt.Sprintf(memberOf, "$1"),
		p.AccountID, *p.IdempotencyKey,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, fmt.Errorf("document: read by key: %w", err)
	}
	if d.ArchivedAt != nil {
		return Document{}, false, &Conflict{Archived: true, Replay: true, Current: d}
	}

	return d, true, nil
}

// insertWorkspace settles which workspace a new document goes in, and that the writer may
// put it there.
func (s *Service) insertWorkspace(ctx context.Context, tx pgx.Tx, p SaveParams) (string, error) {
	workspaceID := p.WorkspaceID

	scope := func(field, query, id string) error {
		var ws string
		err := tx.QueryRow(ctx, query, id).Scan(&ws)
		if errors.Is(err, pgx.ErrNoRows) {
			return &FieldError{Kind: ErrScopeNotFound, Field: field}
		}
		if err != nil {
			return fmt.Errorf("document: read scope: %w", err)
		}
		if workspaceID != "" && workspaceID != ws {
			return &FieldError{Kind: ErrScopeNotFound, Field: field}
		}
		workspaceID = ws

		return nil
	}

	switch {
	case p.TeamID != "":
		if err := scope("team_id", `SELECT workspace_id::text FROM team WHERE id = $1::uuid`, p.TeamID); err != nil {
			return "", err
		}
	case p.ProjectID != "":
		if err := scope("project_id",
			`SELECT tm.workspace_id::text FROM project pr JOIN team tm ON tm.id = pr.team_id
			  WHERE pr.id = $1::uuid`, p.ProjectID); err != nil {
			return "", err
		}
	}

	if p.AccountID == "" {
		return workspaceID, nil
	}

	if workspaceID == "" {
		// ADR 0004: the oldest membership is the account's workspace.
		err := tx.QueryRow(ctx,
			`SELECT w.id::text FROM workspace w
			   JOIN workspace_member wm ON wm.workspace_id = w.id AND wm.account_id = $1::uuid
			  ORDER BY w.created_at, w.id LIMIT 1`, p.AccountID,
		).Scan(&workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", fmt.Errorf("document: read workspace: %w", err)
		}

		return workspaceID, nil
	}

	var member bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspace_member
		                 WHERE workspace_id = $1::uuid AND account_id = $2::uuid)`,
		workspaceID, p.AccountID,
	).Scan(&member); err != nil {
		return "", fmt.Errorf("document: read membership: %w", err)
	}
	if !member {
		// A team or project the caller cannot reach is not found; so is a workspace.
		switch {
		case p.TeamID != "":
			return "", &FieldError{Kind: ErrScopeNotFound, Field: "team_id"}
		case p.ProjectID != "":
			return "", &FieldError{Kind: ErrScopeNotFound, Field: "project_id"}
		}

		return "", ErrNotFound
	}

	return workspaceID, nil
}

// update replaces the row under a lock, so the version it checks is the version it writes
// over: two writers holding the same version both reach the lock, and the second reads the
// first's commit.
func (s *Service) update(ctx context.Context, tx pgx.Tx, p SaveParams, body map[string]any) (Document, map[string]any, *Conversion, error) {
	cur, err := s.lock(ctx, tx, p.ID, p.WorkspaceID, p.AccountID)
	if err != nil {
		return Document{}, nil, nil, err
	}
	if err := cur.refuse(p.IfVersion, false); err != nil {
		return Document{}, nil, nil, err
	}

	if p.MoveProject && p.ProjectID != "" {
		if err := s.checkProject(ctx, tx, cur.WorkspaceID, p.ProjectID); err != nil {
			return Document{}, nil, nil, err
		}
	}

	converting := cur.Type == TypeNormal && p.Type == TypeAspect
	body, conv, err := s.checkBody(ctx, cur.WorkspaceID, p, body, cur.Body, converting)
	if err != nil {
		return Document{}, nil, nil, err
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Document{}, nil, nil, fmt.Errorf("marshal body: %w", err)
	}

	var d Document
	err = d.scan(tx.QueryRow(ctx,
		`WITH d AS (
		     UPDATE document
		        SET body = $1::jsonb, title = $2, type = $3, aspect_type_id = $4::uuid,
		            project_id = CASE WHEN $6::boolean THEN $7::uuid ELSE project_id END,
		            team_id = CASE WHEN NOT $6::boolean THEN team_id
		                           WHEN $7::uuid IS NOT NULL THEN NULL
		                           ELSE COALESCE((SELECT pr.team_id FROM project pr WHERE pr.id = document.project_id), team_id) END,
		            updated_at = now(), version = version + 1
		      WHERE id = $5::uuid
		  RETURNING *)
		 SELECT `+columns+` FROM d`,
		string(raw), p.Title, p.Type, p.aspectType(), p.ID, p.MoveProject, nilIfEmpty(p.ProjectID),
	))
	if err != nil {
		return Document{}, nil, nil, fmt.Errorf("write body: %w", err)
	}

	if err := d.diff(ctx, tx, p.AccountID, cur); err != nil {
		return Document{}, nil, nil, err
	}

	return d, cur.Body, conv, nil
}

// checkProject is that a project a document is moved to exists in the document's workspace.
// Anything else is the same ErrScopeNotFound a create gives, so a project of another
// workspace is indistinguishable from none.
func (s *Service) checkProject(ctx context.Context, tx pgx.Tx, workspaceID, projectID string) error {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM project pr JOIN team tm ON tm.id = pr.team_id
		                 WHERE pr.id = $1::uuid AND tm.workspace_id = $2::uuid)`,
		projectID, workspaceID,
	).Scan(&ok); err != nil {
		return fmt.Errorf("document: read project: %w", err)
	}
	if !ok {
		return &FieldError{Kind: ErrScopeNotFound, Field: "project_id"}
	}

	return nil
}

// memberOf is the caller-scoping predicate, for the one place it is composed into a list.
// lock and the others pass the same two optional scopes as arguments instead.
const memberOf = `EXISTS (SELECT 1 FROM workspace_member wm
	         WHERE wm.workspace_id = d.workspace_id AND wm.account_id = %s::uuid)`

// lock reads a document FOR UPDATE, archived or not, within the scope given. A document
// outside it is ErrNotFound.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, id, workspaceID, accountID string) (Document, error) {
	var d Document
	err := d.scan(tx.QueryRow(ctx,
		`SELECT `+columns+` FROM document d
		  WHERE d.id = $1::uuid
		    AND ($2::uuid IS NULL OR d.workspace_id = $2::uuid)
		    AND ($3::uuid IS NULL OR `+fmt.Sprintf(memberOf, "$3")+`)
		    FOR UPDATE OF d`,
		id, nilIfEmpty(workspaceID), nilIfEmpty(accountID),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("document: lock: %w", err)
	}

	return d, nil
}

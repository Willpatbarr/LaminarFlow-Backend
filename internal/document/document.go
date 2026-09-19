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

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when Save targets a document ID that does not exist.
var ErrNotFound = errors.New("document not found")

// Service is the only type permitted to write document bodies. Handlers depend on
// this; they never reach for the pool themselves.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service backed by pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Document type values. The database enforces this same closed set in
// document_type_is_known; these exist so callers do not spell it themselves.
const (
	TypeNormal = "normal"
	TypeAspect = "aspect"
)

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
	// WorkspaceID scopes the write. Required.
	WorkspaceID string
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
	// Body is the field map. Keys are aspect_type_field IDs.
	Body map[string]any
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
	if p.AspectTypeID == "" {
		return nil
	}
	return &p.AspectTypeID
}

// Save writes a document and regenerates that document's search_index rows in
// one transaction. An empty p.ID inserts a new document into p.WorkspaceID;
// otherwise the existing document is replaced. It returns the document's ID.
//
// Every write is scoped to p.WorkspaceID. Updating a document owned by a
// different workspace returns ErrNotFound rather than a distinct error, so a
// caller outside the owning workspace learns nothing about whether it exists.
//
// This is the single write path required by LAM-3: the blob and the index move
// together or not at all. LAM-23 widened it from the body alone to the whole
// row, so that "the service owns document writes" stays true of the columns
// added around the blob rather than only of the blob. LAM-45 moved the index
// half to internal/search without loosening either property.
//
// Scope columns (project_id, team_id) are deliberately absent. Nothing sets
// them yet, and adding them here would mean choosing how a caller expresses
// "leave the scope alone" versus "clear it" - a question no caller has asked.
func (s *Service) Save(ctx context.Context, p SaveParams) (string, error) {
	p = p.normalize()
	workspaceID, id, body := p.WorkspaceID, p.ID, p.Body
	// Round-trip the body through JSON before extracting text. The rebuild
	// reads values back out of Postgres as decoded JSON, so normalizing here
	// guarantees both paths extract from identical Go values - an int 42 from a
	// handler and a float64 42 from the database cannot diverge.
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal body: %w", err)
	}

	var normalized map[string]any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return "", fmt.Errorf("normalize body: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	// Safe to call unconditionally - a rollback after a successful commit is a
	// no-op. This guarantees no early return can leak an open transaction.
	defer tx.Rollback(ctx)

	if id == "" {
		err = tx.QueryRow(ctx,
			`INSERT INTO document (workspace_id, body, title, type, aspect_type_id)
			 VALUES ($1::uuid, $2::jsonb, $3, $4, $5::uuid) RETURNING id::text`,
			workspaceID, string(raw), p.Title, p.Type, p.aspectType(),
		).Scan(&id)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE document
			    SET body = $1::jsonb, title = $2, type = $3,
			        aspect_type_id = $4::uuid, updated_at = now()
			 WHERE id = $5::uuid AND workspace_id = $6::uuid
			 RETURNING id::text`,
			string(raw), p.Title, p.Type, p.aspectType(), id, workspaceID,
		).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("write body: %w", err)
	}

	// Both index calls take this transaction, so the blob and its rows still move
	// together or not at all. That invariant is the point of Save and survived the
	// move to internal/search unchanged.
	if err := search.ClearDocument(ctx, tx, id); err != nil {
		return "", err
	}

	if err := search.IndexDocument(ctx, tx, id, normalized); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	return id, nil
}

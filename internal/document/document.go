package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when Save targets a document ID that does not exist.
var ErrNotFound = errors.New("document not found")

// Service is the only type permitted to write document bodies or search_index
// rows. Handlers depend on this; they never reach for the pool themselves.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service backed by pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Save writes a document's body and regenerates that document's search_index
// rows in one transaction. An empty id inserts a new document; otherwise the
// existing document's body is replaced. It returns the document's ID.
//
// This is the single write path required by LAM-3: the blob and the index move
// together or not at all.
func (s *Service) Save(ctx context.Context, id string, body map[string]any) (string, error) {
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
			`INSERT INTO document (body) VALUES ($1::jsonb) RETURNING id::text`,
			string(raw),
		).Scan(&id)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE document SET body = $1::jsonb, updated_at = now()
			 WHERE id = $2::uuid RETURNING id::text`,
			string(raw), id,
		).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("write body: %w", err)
	}

	// Delete-then-insert rather than upsert: a field removed from the body must
	// lose its index row. An upsert alone leaves that row behind forever, and
	// the rebuild would then legitimately disagree with the live index - a
	// drift bug that presents as a rebuild bug.
	if _, err := tx.Exec(ctx,
		`DELETE FROM search_index WHERE document_id = $1::uuid`, id,
	); err != nil {
		return "", fmt.Errorf("clear index: %w", err)
	}

	for fieldID, value := range normalized {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_index (document_id, field_id, content)
			 VALUES ($1::uuid, $2, $3)`,
			id, fieldID, FieldText(value),
		); err != nil {
			return "", fmt.Errorf("index field %q: %w", fieldID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	return id, nil
}


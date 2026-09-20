/*
╔═ search.go ═══════════════════════════════════════════════════════════════════════════
║  search · the only writer of search_index
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service      struct
║      Counts       struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/document.Save  →  IndexDocument, inside its transaction
║      cmd/reindex             →  Rebuild
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package search owns search_index.
//
// It was internal/document's, which was right while the index was one document's
// derived data. LAM-45 added tickets and comments as sources, and an index derived
// from three tables is nobody's derived data in particular - so it moved here rather
// than leaving internal/document owning rows about tickets.
//
// Reads, not writes, of the source tables. This package denormalises scope from
// document, ticket, comment, project and team so each row carries the scope of the
// thing it describes; it writes none of them. sqlguard enforces exactly that split.
package search

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  owns search_index, reads every source it indexes
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Rebuild(ctx)              →  Counts, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service writing search_index through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

/*
┏━ Counts ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what a rebuild regenerated, per source
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Documents    int          documents, not rows
┃      Tickets      int
┃      Comments     int
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Rebuild                   one per run
*/

type Counts struct {
	Documents int
	Tickets   int
	Comments  int
}

/*
┌─ search ────────────────────────────────────────
│  discards every index row and regenerates all three sources
├─ in ────────────────────────────────────────────
│      ctx    context.Context
├─ out ───────────────────────────────────────────
│      Counts             per source, documents not rows
│      error
├─ example ───────────────────────────────────────
│      3 docs, 2 tickets  →  Counts{3, 2, 0}
*/

// Rebuild is the sanctioned exception ADR 0001 names: it writes the index without a
// source write, because it is a pure function of the sources. If it ever produces a
// different index than the live path, the live path was wrong.
//
// Instance-wide on purpose, like the version it replaces. That is the right scope for
// an operator repairing a derived table, and the wrong thing to expose to a caller
// acting inside one workspace.
func (s *Service) Rebuild(ctx context.Context) (Counts, error) {
	var counts Counts

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return counts, fmt.Errorf("search: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM search_index`); err != nil {
		return counts, fmt.Errorf("search: clear index: %w", err)
	}

	// Documents first, and one at a time, because their text comes from Go rather
	// than SQL: fieldText walks a decoded body. Tickets and comments index plain
	// columns, so each is a single set-based insert.
	docs, err := readDocumentBodies(ctx, tx)
	if err != nil {
		return counts, err
	}
	for _, d := range docs {
		if err := indexDocument(ctx, tx, d.id, d.body); err != nil {
			return counts, fmt.Errorf("search: index document %s: %w", d.id, err)
		}
	}
	counts.Documents = len(docs)

	if counts.Tickets, err = indexAllTickets(ctx, tx); err != nil {
		return counts, err
	}
	if counts.Comments, err = indexAllComments(ctx, tx); err != nil {
		return counts, err
	}

	if err := tx.Commit(ctx); err != nil {
		return counts, fmt.Errorf("search: commit: %w", err)
	}

	return counts, nil
}

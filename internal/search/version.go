package search

import (
	"context"
	"fmt"
	"log"
)

// IndexVersion is the format of the text in search_index. Raise it whenever the way
// text is extracted changes, so the next start rebuilds rows written the old way.
//
//	1  Markdown reduced to plain text before indexing (LAM-143)
const IndexVersion = 1

// advisoryLockKey serialises EnsureCurrent across instances. Arbitrary, but fixed.
const advisoryLockKey = 0x6c66_7365_6172_6368 // "lfsearch"

// EnsureCurrent rebuilds the index when the stored version is older than IndexVersion,
// then stores IndexVersion; otherwise it does nothing. rebuilt says which happened.
//
// It is one transaction holding an advisory lock, so two instances starting together
// rebuild once: the second waits on the lock, then reads the version the first stored
// and returns. If the rebuild fails nothing is stored (the transaction rolls back, old
// index and old version both), so the next start tries again.
func (s *Service) EnsureCurrent(ctx context.Context) (rebuilt bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("search: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(advisoryLockKey)); err != nil {
		return false, fmt.Errorf("search: lock: %w", err)
	}

	var stored int
	if err := tx.QueryRow(ctx, `SELECT version FROM search_index_version`).Scan(&stored); err != nil {
		return false, fmt.Errorf("search: read index version: %w", err)
	}
	if stored >= IndexVersion {
		return false, nil
	}

	log.Printf("search: index version %d is older than %d, rebuilding", stored, IndexVersion)
	counts, err := rebuildIn(ctx, tx)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE search_index_version SET version = $1`, IndexVersion); err != nil {
		return false, fmt.Errorf("search: store index version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("search: commit: %w", err)
	}

	log.Printf("search: rebuilt index to version %d: %d documents, %d tickets, %d comments, %d projects",
		IndexVersion, counts.Documents, counts.Tickets, counts.Comments, counts.Projects)

	return true, nil
}

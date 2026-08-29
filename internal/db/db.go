// Package db owns the connection to Postgres. The Go backend is the only
// component that talks to Postgres directly - the frontend goes through the
// HTTP API and never holds a driver or a connection string.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const connectTimeout = 5 * time.Second

// Connect opens a connection pool and verifies Postgres is actually reachable
// before returning, so a misconfigured URL fails at startup rather than on the
// first request.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return pool, nil
}

// Check runs a trivial query to confirm the connection is still usable.
func Check(ctx context.Context, pool *pgxpool.Pool) error {
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("select 1: %w", err)
	}

	if one != 1 {
		return fmt.Errorf("select 1 returned %d, want 1", one)
	}

	return nil
}

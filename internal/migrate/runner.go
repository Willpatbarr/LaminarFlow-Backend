package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// createTable is applied before every operation. schema_migrations is the one
// table the runner owns, and it has to exist before the runner can ask what has
// already run - so creating it is not itself a migration.
const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    bigint      PRIMARY KEY,
    name       text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// withLock acquires a connection, takes the advisory lock, guarantees
// schema_migrations exists, and runs fn.
//
// The lock is what makes two servers starting at once safe: without it both can
// read the same pending list and both try to apply migration 4, and the loser
// fails with a confusing "table already exists" rather than simply waiting.
func withLock(
	ctx context.Context,
	pool *pgxpool.Pool,
	fn func(context.Context, *pgxpool.Conn) error,
) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("take advisory lock: %w", err)
	}
	defer func() {
		// WithoutCancel so a cancelled context still releases the lock. Leaving
		// it held would block every later run on this connection's lifetime.
		unlockCtx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
			// Nothing useful to do here - the session ending releases it anyway.
			_ = err
		}
	}()

	if _, err := conn.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	return fn(ctx, conn)
}

// appliedVersions reads schema_migrations into a set.
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]bool)
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}

	return applied, nil
}

// Up applies every migration not yet recorded, oldest first, and returns the
// ones it applied. Running it twice in a row is a no-op the second time.
func Up(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]Migration, error) {
	var done []Migration

	err := withLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}

		for _, m := range migrations {
			if applied[m.Version] {
				continue
			}
			if err := apply(ctx, conn, m, m.Up, true); err != nil {
				return err
			}
			done = append(done, m)
		}

		return nil
	})

	return done, err
}

// Down rolls back the n most recently applied migrations, newest first. n <= 0
// rolls back exactly one, because "down" with no count meaning "destroy the
// whole schema" is a bad default.
func Down(ctx context.Context, pool *pgxpool.Pool, migrations []Migration, n int) ([]Migration, error) {
	if n <= 0 {
		n = 1
	}

	var done []Migration

	err := withLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}

		// Newest first: a migration can depend on an earlier one, so unwinding
		// in application order would drop a table something still references.
		for i := len(migrations) - 1; i >= 0 && len(done) < n; i-- {
			m := migrations[i]
			if !applied[m.Version] {
				continue
			}
			if m.Down == "" {
				return fmt.Errorf("%04d_%s: %w", m.Version, m.Name, ErrIrreversible)
			}
			if err := apply(ctx, conn, m, m.Down, false); err != nil {
				return err
			}
			done = append(done, m)
		}

		return nil
	})

	return done, err
}

// apply runs one migration's SQL and records or unrecords it, in a single
// transaction. Postgres has transactional DDL, so a failure part-way through
// leaves neither the schema change nor the bookkeeping behind - the two can
// never disagree about what has run.
func apply(ctx context.Context, conn *pgxpool.Conn, m Migration, sql string, up bool) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// No arguments, so pgx sends this over the simple protocol - which is what
	// lets one migration file hold several statements, the same semantics as
	// psql -1 -f.
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%04d_%s: %w", m.Version, m.Name, err)
	}

	if up {
		_, err = tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
			m.Version, m.Name)
	} else {
		_, err = tx.Exec(ctx,
			`DELETE FROM schema_migrations WHERE version = $1`, m.Version)
	}
	if err != nil {
		return fmt.Errorf("record %04d_%s: %w", m.Version, m.Name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %04d_%s: %w", m.Version, m.Name, err)
	}

	return nil
}

// Status reports every known migration and whether it has been applied, plus
// any version recorded in the database that no longer has a file. The second
// case means someone deleted a migration that has already run somewhere, which
// the runner cannot undo and should not hide.
func Status(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]Record, error) {
	var records []Record

	err := withLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}

		known := make(map[int64]bool, len(migrations))
		for _, m := range migrations {
			known[m.Version] = true
			records = append(records, Record{
				Version: m.Version,
				Name:    m.Name,
				Applied: applied[m.Version],
			})
		}

		for v := range applied {
			if !known[v] {
				records = append(records, Record{Version: v, Name: "(file missing)", Applied: true})
			}
		}

		return nil
	})

	return records, err
}

// Pending returns the migrations that Up would apply. main.go uses it to refuse
// to serve against a schema it does not match, rather than failing later on the
// first query that hits a missing column.
func Pending(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]Migration, error) {
	var pending []Migration

	err := withLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		for _, m := range migrations {
			if !applied[m.Version] {
				pending = append(pending, m)
			}
		}
		return nil
	})

	return pending, err
}

// Baseline records every migration as applied without running any of them.
//
// This exists for exactly one situation: a database whose schema was built by
// hand before this runner existed, which is where LAM-2 through LAM-5 left the
// development database. Running Up there would try to CREATE TABLE document a
// second time and fail. It refuses on a database that already has any recorded
// migration, so it cannot be used to paper over a genuinely failed run.
func Baseline(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]Migration, error) {
	var done []Migration

	err := withLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		if len(applied) > 0 {
			return errors.New("database already has recorded migrations - baseline is only for adopting a hand-built schema")
		}

		batch := &pgx.Batch{}
		for _, m := range migrations {
			batch.Queue(`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
				m.Version, m.Name)
			done = append(done, m)
		}

		if err := conn.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("record baseline: %w", err)
		}

		return nil
	})

	return done, err
}

package migrate

import (
	"context"
	"testing"
)

// The constraints 0005_team.sql claims, each asserted by trying to violate it.
// Subtests share one throwaway database and take a workspace each, rather than
// paying to build seven databases for seven assertions.
func TestTeamConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	t.Run("rejects a team in a workspace that does not exist", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')`,
			"00000000-0000-0000-0000-000000000000")
		wantPgError(t, err, foreignKeyViolation, "orphan team")
	})

	t.Run("rejects a null workspace", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES (NULL, 'Platform')`)
		wantPgError(t, err, notNullViolation, "null workspace_id")
	})

	t.Run("rejects a null name", func(t *testing.T) {
		ws := newWorkspace(t, pool, "null-name")

		_, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, NULL)`, ws)
		wantPgError(t, err, notNullViolation, "null name")
	})

	t.Run("rejects one name twice in one workspace", func(t *testing.T) {
		ws := newWorkspace(t, pool, "duplicate")

		if _, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')`, ws,
		); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		_, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')`, ws)
		wantPgError(t, err, uniqueViolation, "duplicate team name")
	})

	// The half that carries the claim. The subtest above passes just as
	// happily against a global UNIQUE (name), so without this one "unique
	// within a workspace, not globally" is asserted nowhere.
	t.Run("allows one name in two workspaces", func(t *testing.T) {
		for _, ws := range []string{
			newWorkspace(t, pool, "scoped-one"),
			newWorkspace(t, pool, "scoped-two"),
		} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Design')`, ws,
			); err != nil {
				t.Fatalf("insert into workspace %s: %v", ws, err)
			}
		}
	})

	// Nothing in the Go deletes a workspace yet, so this is the only thing
	// standing between ON DELETE CASCADE and ON DELETE RESTRICT.
	t.Run("deleting a workspace deletes its teams", func(t *testing.T) {
		ws := newWorkspace(t, pool, "cascade")

		if _, err := pool.Exec(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Doomed')`, ws,
		); err != nil {
			t.Fatalf("insert team: %v", err)
		}

		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, ws,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		var teams int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM team WHERE workspace_id = $1::uuid`, ws,
		).Scan(&teams); err != nil {
			t.Fatalf("count teams: %v", err)
		}
		if teams != 0 {
			t.Errorf("%d teams survived their workspace, want 0", teams)
		}
	})

	// 0005_team.sql declines to add a separate index on workspace_id, on the
	// grounds that UNIQUE (workspace_id, name) indexes the column already and
	// a btree index serves lookups on its leading column. That reasoning holds
	// only while workspace_id is actually the leading column - reorder the
	// constraint to (name, workspace_id) and the foreign key silently loses
	// its index.
	t.Run("workspace_id leads an index", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid
                    AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'team'::regclass
                    AND a.attname = 'workspace_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on team leads with workspace_id, so the foreign key is unindexed")
		}
	})
}

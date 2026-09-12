package document

import (
	"context"
	"testing"
)

// The constraints 0003_workspace.sql claims, and the ones it deliberately
// declines.
//
// This file exists because the epic-close audit found workspace to be the one
// table in the schema with no constraint test of its own. That is a
// consequence of how LAM-11 closed: migration 0003 had already created the
// table under LAM-4, so LAM-11 produced no commit, and the playbook's "write
// the tests the migration earns" step was skipped along with it. The table
// still makes claims, and two of them are load-bearing for tests in other
// files that would fail confusingly if either ever changed.
//
// It lives in internal/document rather than in internal/migrate beside most
// tables' constraint tests, for the reason LAM-23's and LAM-24's do: the most
// consequential thing workspace claims is the RESTRICT on
// document.workspace_id, and asserting it means inserting a document.
// TestNoSQLOutsideThisPackage bars every other package from issuing SQL
// against document, so the whole file is kept here rather than split across
// two packages to save one subtest.
func TestWorkspaceConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	newWorkspace := func(t *testing.T, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO workspace (name) VALUES ($1) RETURNING id::text`, name,
		).Scan(&id); err != nil {
			t.Fatalf("create workspace %s: %v", name, err)
		}
		t.Cleanup(func() {
			// workspace rows outlive testPool's DELETE FROM document, so a
			// subtest that makes one has to take it away again or the next
			// run starts with a different number of workspaces than the last.
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM workspace WHERE id = $1::uuid`, id); err != nil {
				t.Errorf("clean up workspace %s: %v", name, err)
			}
		})

		return id
	}

	// The fresh-install path. LAM-11 closed on the argument that 0003's seed
	// already satisfies "first-run/bootstrap logic" - ensureSchema applies
	// migrations at startup and TestMain applies them to an empty database on
	// every run, so the INSERT is on the first-run path already, inside the
	// same transaction as the CREATE TABLE. Nothing asserted that until now,
	// which made the whole argument unfalsifiable.
	t.Run("migration 0003 seeds exactly one Default workspace", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace WHERE name = 'Default'`,
		).Scan(&n); err != nil {
			t.Fatalf("count Default workspaces: %v", err)
		}
		if n != 1 {
			t.Errorf("%d workspaces named 'Default', want exactly 1 - "+
				"0003's seed is the fresh-install bootstrap and defaultWorkspace() depends on it", n)
		}
	})

	t.Run("rejects a null name", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO workspace (name) VALUES (NULL)`)
		wantPgError(t, err, notNullViolation, "a null name")
	})

	t.Run("fills in id and both timestamps", func(t *testing.T) {
		id := newWorkspace(t, "ws-defaults")

		var created, updated *string
		if err := pool.QueryRow(ctx,
			`SELECT created_at::text, updated_at::text FROM workspace WHERE id = $1::uuid`, id,
		).Scan(&created, &updated); err != nil {
			t.Fatalf("read the workspace back: %v", err)
		}
		if created == nil || updated == nil {
			t.Errorf("created_at=%v updated_at=%v, want both filled in", created, updated)
		}
	})

	// The declined constraint, and the reason it is worth pinning. 0003's why
	// block says nothing may assume there is exactly one workspace, and two
	// things in this repo depend on that being true rather than merely
	// intended: document_test.go inserts a second workspace to test the
	// tenancy boundary, and internal/migrate's newWorkspace helper builds one
	// per subtest so that tables constrained on (workspace_id, name) can be
	// tested without colliding. Adding UNIQUE (name) or a one-row CHECK here
	// would break both, in another package, with an error naming neither.
	t.Run("accepts a second workspace, and two sharing a name", func(t *testing.T) {
		newWorkspace(t, "Default")

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace WHERE name = 'Default'`,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Errorf("%d workspaces named 'Default', want 2 - workspace.name is "+
				"deliberately not unique and nothing enforces a single workspace", n)
		}
	})

	// The one workspace constraint that reaches another table, and the reason
	// this file is in internal/document. RESTRICT, not CASCADE: deleting a
	// workspace that still holds documents is blocked rather than destroying
	// them. Every other parent in this schema cascades, so this is the
	// exception and the easiest one to "tidy up" into consistency later.
	t.Run("blocks deleting a workspace that still holds documents", func(t *testing.T) {
		ws := newWorkspace(t, "ws-restrict")

		var doc string
		if err := pool.QueryRow(ctx,
			`INSERT INTO document (workspace_id) VALUES ($1::uuid) RETURNING id::text`, ws,
		).Scan(&doc); err != nil {
			t.Fatalf("create document: %v", err)
		}

		_, err := pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, ws)
		wantPgError(t, err, foreignKeyViolation, "deleting a workspace that still holds documents")

		// ...and the block lifts once the documents are gone, so RESTRICT is
		// not a permanent lock on the row.
		if _, err := pool.Exec(ctx, `DELETE FROM document WHERE id = $1::uuid`, doc); err != nil {
			t.Fatalf("delete the document: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, ws); err != nil {
			t.Fatalf("delete the emptied workspace: %v", err)
		}
	})
}

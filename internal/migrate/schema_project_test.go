package migrate

import (
	"context"
	"testing"
)

// The constraints 0006_project.sql claims, each asserted by trying to violate
// it. Subtests share one throwaway database and take a workspace and team
// each.
func TestProjectConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	t.Run("rejects a project in a team that does not exist", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')`,
			"00000000-0000-0000-0000-000000000000")
		wantPgError(t, err, foreignKeyViolation, "orphan project")
	})

	t.Run("rejects a null team", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO project (team_id, name) VALUES (NULL, 'Apollo')`)
		wantPgError(t, err, notNullViolation, "null team_id")
	})

	t.Run("rejects a null name", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "null-name"), "Platform")

		_, err := pool.Exec(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, NULL)`, team)
		wantPgError(t, err, notNullViolation, "null name")
	})

	// 0006_project.sql deliberately has no UNIQUE (team_id, name), because
	// LAM-13 does not ask for one. This asserts the absence rather than
	// leaving it to be inferred: if uniqueness is ever added, this subtest
	// fails and forces the change to be deliberate.
	t.Run("allows one name twice in one team", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "duplicate"), "Platform")

		for i := 0; i < 2; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')`, team,
			); err != nil {
				t.Fatalf("insert %d: %v", i+1, err)
			}
		}
	})

	t.Run("deleting a team deletes its projects", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "cascade-team"), "Platform")

		if _, err := pool.Exec(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Doomed')`, team,
		); err != nil {
			t.Fatalf("insert project: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}

		var projects int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM project WHERE team_id = $1::uuid`, team,
		).Scan(&projects); err != nil {
			t.Fatalf("count projects: %v", err)
		}
		if projects != 0 {
			t.Errorf("%d projects survived their team, want 0", projects)
		}
	})

	// The chain, not just the link. team.workspace_id and project.team_id are
	// both CASCADE, so deleting a workspace has to reach a project two levels
	// down. Neither table's own cascade test would catch a break in the other.
	t.Run("deleting a workspace cascades through team to project", func(t *testing.T) {
		ws := newWorkspace(t, pool, "cascade-chain")
		team := newTeam(t, pool, ws, "Platform")

		if _, err := pool.Exec(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Doomed')`, team,
		); err != nil {
			t.Fatalf("insert project: %v", err)
		}

		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, ws,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		var projects int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM project WHERE team_id = $1::uuid`, team,
		).Scan(&projects); err != nil {
			t.Fatalf("count projects: %v", err)
		}
		if projects != 0 {
			t.Errorf("%d projects survived their workspace, want 0", projects)
		}
	})

	// LAM-13 step 2 asks for an index on team_id, and unlike team there is no
	// UNIQUE constraint to supply one, so project_team_id_idx is the only
	// thing standing between a foreign key lookup and a sequential scan.
	t.Run("team_id leads an index", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid
                    AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'project'::regclass
                    AND a.attname = 'team_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on project leads with team_id, so the foreign key is unindexed")
		}
	})
}

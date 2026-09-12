package migrate

import (
	"context"
	"testing"
)

// The constraints 0009_setting.sql claims, each asserted by trying to violate
// it. The scope is an exclusive arc, so the interesting cases are the two the
// CHECK rejects - no scope and both scopes - and the fact that workspace and
// team keys are independent namespaces.
func TestSettingConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// scope is written as a pair so a caller can pass exactly one, or
	// deliberately pass none or both to test the CHECK.
	insert := func(workspaceID, teamID *string, key, value string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO setting (workspace_id, team_id, key, value)
             VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)`,
			workspaceID, teamID, key, value)
		return err
	}
	ptr := func(s string) *string { return &s }

	t.Run("rejects a setting with no scope", func(t *testing.T) {
		wantPgError(t, insert(nil, nil, "theme", `"dark"`),
			checkViolation, "no scope")
	})

	t.Run("rejects a setting with both scopes", func(t *testing.T) {
		ws := newWorkspace(t, pool, "both-scopes")
		team := newTeam(t, pool, ws, "Platform")

		wantPgError(t, insert(ptr(ws), ptr(team), "theme", `"dark"`),
			checkViolation, "two scopes")
	})

	t.Run("accepts a workspace scope and a team scope", func(t *testing.T) {
		ws := newWorkspace(t, pool, "happy-path")
		team := newTeam(t, pool, ws, "Platform")

		if err := insert(ptr(ws), nil, "theme", `"dark"`); err != nil {
			t.Fatalf("workspace-scoped: %v", err)
		}
		if err := insert(nil, ptr(team), "theme", `"light"`); err != nil {
			t.Fatalf("team-scoped: %v", err)
		}
	})

	t.Run("rejects an orphan workspace", func(t *testing.T) {
		wantPgError(t,
			insert(ptr("00000000-0000-0000-0000-000000000000"), nil, "theme", `"dark"`),
			foreignKeyViolation, "orphan workspace_id")
	})

	t.Run("rejects an orphan team", func(t *testing.T) {
		wantPgError(t,
			insert(nil, ptr("00000000-0000-0000-0000-000000000000"), "theme", `"dark"`),
			foreignKeyViolation, "orphan team_id")
	})

	t.Run("rejects a duplicate key in one workspace", func(t *testing.T) {
		ws := newWorkspace(t, pool, "dup-workspace")

		if err := insert(ptr(ws), nil, "theme", `"dark"`); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		wantPgError(t, insert(ptr(ws), nil, "theme", `"light"`),
			uniqueViolation, "duplicate workspace key")
	})

	// The case a plain UNIQUE (workspace_id, team_id, key) would let through.
	// Postgres treats NULLs as distinct in a unique constraint, so these two
	// rows - identical but for their never-equal workspace_id NULLs - would
	// both be accepted. The partial index is what rejects them.
	t.Run("rejects a duplicate key in one team", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "dup-team"), "Platform")

		if err := insert(nil, ptr(team), "columns", `["To Do"]`); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		wantPgError(t, insert(nil, ptr(team), "columns", `["Doing"]`),
			uniqueViolation, "duplicate team key")
	})

	t.Run("allows one key in two workspaces", func(t *testing.T) {
		for _, ws := range []string{
			newWorkspace(t, pool, "scoped-one"),
			newWorkspace(t, pool, "scoped-two"),
		} {
			if err := insert(ptr(ws), nil, "theme", `"dark"`); err != nil {
				t.Fatalf("insert into workspace %s: %v", ws, err)
			}
		}
	})

	// Workspace settings and team settings are independent namespaces: a team
	// overriding a workspace-wide key is the whole point of having both.
	t.Run("allows one key at workspace and team scope", func(t *testing.T) {
		ws := newWorkspace(t, pool, "override")
		team := newTeam(t, pool, ws, "Platform")

		if err := insert(ptr(ws), nil, "columns", `["To Do","Done"]`); err != nil {
			t.Fatalf("workspace-scoped: %v", err)
		}
		if err := insert(nil, ptr(team), "columns", `["Backlog","Doing","Done"]`); err != nil {
			t.Fatalf("team-scoped override: %v", err)
		}
	})

	t.Run("rejects a null key and a null value", func(t *testing.T) {
		ws := newWorkspace(t, pool, "nulls")

		_, err := pool.Exec(ctx,
			`INSERT INTO setting (workspace_id, key, value)
             VALUES ($1::uuid, NULL, '"dark"'::jsonb)`, ws)
		wantPgError(t, err, notNullViolation, "null key")

		_, err = pool.Exec(ctx,
			`INSERT INTO setting (workspace_id, key, value)
             VALUES ($1::uuid, 'theme', NULL)`, ws)
		wantPgError(t, err, notNullViolation, "null value")
	})

	// What LAM-16 step 3 wanted a seeded row to prove, without seeding one:
	// the shape really does hold a list, a scalar and an object.
	t.Run("value holds a list, a scalar and an object", func(t *testing.T) {
		ws := newWorkspace(t, pool, "value-shapes")

		shapes := map[string]string{
			"columns":     `["Backlog","Doing","Done"]`,
			"sprint_days": `14`,
			"wip_limits":  `{"Doing":3}`,
		}
		for key, value := range shapes {
			if err := insert(ptr(ws), nil, key, value); err != nil {
				t.Fatalf("insert %s: %v", key, err)
			}
		}

		var columns []string
		if err := pool.QueryRow(ctx,
			`SELECT ARRAY(SELECT jsonb_array_elements_text(value))
               FROM setting WHERE workspace_id = $1::uuid AND key = 'columns'`, ws,
		).Scan(&columns); err != nil {
			t.Fatalf("read columns back: %v", err)
		}
		if len(columns) != 3 || columns[0] != "Backlog" {
			t.Errorf("read back %v, want the three Kanban columns", columns)
		}
	})

	t.Run("deleting a workspace deletes its settings", func(t *testing.T) {
		ws := newWorkspace(t, pool, "cascade-workspace")

		if err := insert(ptr(ws), nil, "theme", `"dark"`); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, ws); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		var settings int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM setting WHERE workspace_id = $1::uuid`, ws,
		).Scan(&settings); err != nil {
			t.Fatalf("count settings: %v", err)
		}
		if settings != 0 {
			t.Errorf("%d settings survived their workspace, want 0", settings)
		}
	})

	t.Run("deleting a team deletes its settings", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "cascade-team"), "Platform")

		if err := insert(nil, ptr(team), "columns", `["To Do"]`); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}

		var settings int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM setting WHERE team_id = $1::uuid`, team,
		).Scan(&settings); err != nil {
			t.Fatalf("count settings: %v", err)
		}
		if settings != 0 {
			t.Errorf("%d settings survived their team, want 0", settings)
		}
	})
}

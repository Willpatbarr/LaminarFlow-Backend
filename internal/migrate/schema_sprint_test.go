package migrate

import (
	"context"
	"testing"
)

// The constraints 0012_sprint.sql claims. Half of this file asserts absences:
// sprints may overlap, may share a name, and may have no dates at all, and
// each of those is a decision rather than an oversight.
func TestSprintConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	newProject := func(t *testing.T, label string) string {
		t.Helper()

		team := newTeam(t, pool, newWorkspace(t, pool, label), "Platform")

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&id); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return id
	}
	insert := func(projectID, name string, start, end *string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO sprint (project_id, name, start_date, end_date)
             VALUES ($1::uuid, $2, $3::date, $4::date)`,
			projectID, name, start, end)
		return err
	}
	day := func(s string) *string { return &s }
	countSprints := func(t *testing.T, projectID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM sprint WHERE project_id = $1::uuid`, projectID,
		).Scan(&n); err != nil {
			t.Fatalf("count sprints: %v", err)
		}

		return n
	}

	t.Run("rejects a sprint in a project that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "Orphan", nil, nil)
		wantPgError(t, err, foreignKeyViolation, "orphan sprint")
	})

	t.Run("rejects a null project and a null name", func(t *testing.T) {
		project := newProject(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO sprint (project_id, name) VALUES (NULL, 'Sprint 1')`)
		wantPgError(t, err, notNullViolation, "null project_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO sprint (project_id, name) VALUES ($1::uuid, NULL)`, project)
		wantPgError(t, err, notNullViolation, "null name")
	})

	// A sprint being planned has a name before it has dates.
	t.Run("accepts a sprint with no dates", func(t *testing.T) {
		project := newProject(t, "draft")

		if err := insert(project, "Someday", nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// The CHECK has to tolerate a half-filled timebox, and it does so without
	// any IS NULL clause: SQL evaluates it to NULL when either side is
	// missing, and a CHECK only rejects on false.
	t.Run("accepts a sprint with a start and no end", func(t *testing.T) {
		project := newProject(t, "half-dated")

		if err := insert(project, "Started", day("2026-09-01"), nil); err != nil {
			t.Fatalf("start only: %v", err)
		}
		if err := insert(project, "Ending", nil, day("2026-09-14")); err != nil {
			t.Fatalf("end only: %v", err)
		}
	})

	t.Run("rejects a sprint that ends before it starts", func(t *testing.T) {
		project := newProject(t, "backwards")

		wantPgError(t,
			insert(project, "Backwards", day("2026-09-14"), day("2026-09-01")),
			checkViolation, "end before start")
	})

	t.Run("accepts a one-day sprint", func(t *testing.T) {
		project := newProject(t, "one-day")

		if err := insert(project, "Hack day", day("2026-09-01"), day("2026-09-01")); err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Deliberate absence. A team may run parallel tracks, or leave one sprint
	// open past the start of the next. An EXCLUDE over daterange would forbid
	// this, and LAM-19 does not ask for that rule.
	t.Run("allows two sprints to overlap", func(t *testing.T) {
		project := newProject(t, "overlap")

		if err := insert(project, "Sprint 1", day("2026-09-01"), day("2026-09-14")); err != nil {
			t.Fatalf("first sprint: %v", err)
		}
		if err := insert(project, "Sprint 2", day("2026-09-08"), day("2026-09-21")); err != nil {
			t.Fatalf("overlapping sprint: %v", err)
		}
	})

	// Deliberate absence, matching project and status.
	t.Run("allows one name twice in one project", func(t *testing.T) {
		project := newProject(t, "duplicate-name")

		for i := 0; i < 2; i++ {
			if err := insert(project, "Sprint 1", nil, nil); err != nil {
				t.Fatalf("insert %d: %v", i+1, err)
			}
		}
	})

	t.Run("deleting a project deletes its sprints", func(t *testing.T) {
		project := newProject(t, "project-cascade")

		if err := insert(project, "Doomed", nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete project: %v", err)
		}

		if got := countSprints(t, project); got != 0 {
			t.Errorf("%d sprints survived their project, want 0", got)
		}
	})

	// workspace to team to project to sprint. Four levels, and no single
	// table's own test covers a break in another's.
	t.Run("deleting a workspace cascades all the way to the sprint", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "full-chain")
		team := newTeam(t, pool, workspace, "Platform")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := insert(project, "Doomed", nil, nil); err != nil {
			t.Fatalf("insert sprint: %v", err)
		}

		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		if got := countSprints(t, project); got != 0 {
			t.Errorf("%d sprints survived their workspace, want 0", got)
		}
	})

	t.Run("project_id leads an index and start_date follows", func(t *testing.T) {
		var covers bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute lead
                     ON lead.attrelid = i.indrelid AND lead.attnum = i.indkey[0]
                   JOIN pg_attribute second
                     ON second.attrelid = i.indrelid AND second.attnum = i.indkey[1]
                  WHERE i.indrelid = 'sprint'::regclass
                    AND lead.attname = 'project_id'
                    AND second.attname = 'start_date'
             )`,
		).Scan(&covers); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !covers {
			t.Error("no index on sprint leads with project_id followed by start_date")
		}
	})
}

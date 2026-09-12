package migrate

import (
	"context"
	"testing"
)

// The constraints 0025_board.sql claims for board itself. Two of them matter:
// the composite reference that fixes the team at the top of the board chain,
// and the group_by CHECK that keeps the column-source set closed while it
// holds exactly one value.
func TestBoardConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// "bd-" namespaced per file - see schema_test.go.
	newTeamProject := func(t *testing.T, label string) (team, project string) {
		t.Helper()

		team = newTeam(t, pool, newWorkspace(t, pool, "bd-"+label), "Team bd-"+label)

		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return team, project
	}
	insert := func(projectID, teamID, name string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, $3)`, projectID, teamID, name)
		return err
	}
	countBoards := func(t *testing.T, projectID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM board WHERE project_id = $1::uuid`, projectID,
		).Scan(&n); err != nil {
			t.Fatalf("count boards: %v", err)
		}

		return n
	}

	t.Run("rejects a board whose project does not exist", func(t *testing.T) {
		team, _ := newTeamProject(t, "no-such-project")

		err := insert("00000000-0000-0000-0000-000000000000", team, "Sprint board")
		wantPgError(t, err, foreignKeyViolation, "a board with no project")
	})

	// The top hop of the team chain, on its own: a real project and a real
	// team that are not parent and child.
	t.Run("rejects a project and a team that are not parent and child", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bd-mismatched")
		home := newTeam(t, pool, workspace, "Team bd-mismatch-home")
		other := newTeam(t, pool, workspace, "Team bd-mismatch-other")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, home,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		err := insert(project, other, "Sprint board")
		wantPgError(t, err, foreignKeyViolation, "a board naming a team that does not own its project")
	})

	t.Run("rejects a null project, team or name", func(t *testing.T) {
		team, project := newTeamProject(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name) VALUES (NULL, $1::uuid, 'B')`, team)
		wantPgError(t, err, notNullViolation, "a null project_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name) VALUES ($1::uuid, NULL, 'B')`, project)
		wantPgError(t, err, notNullViolation, "a null team_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name) VALUES ($1::uuid, $2::uuid, NULL)`,
			project, team)
		wantPgError(t, err, notNullViolation, "a null name")
	})

	// Closed set, for status.category's reason: every renderer branches on
	// it, so a typo must fail at write time rather than drop a board out of
	// every view.
	t.Run("rejects an unknown group_by", func(t *testing.T) {
		team, project := newTeamProject(t, "unknown-group-by")

		for _, value := range []string{"statuses", "Status", "", "label"} {
			_, err := pool.Exec(ctx,
				`INSERT INTO board (project_id, team_id, name, group_by)
                 VALUES ($1::uuid, $2::uuid, 'Sprint board', $3)`, project, team, value)
			wantPgError(t, err, checkViolation, "group_by = "+value)
		}
	})

	// 'label' is deliberately not in the CHECK yet even though LAM-49 shipped
	// the table two migrations ago: board_column_status is status-specific,
	// so widening the set without a matching mapping table would allow a
	// board that cannot render its own columns.
	t.Run("defaults group_by to status, the only value there is", func(t *testing.T) {
		team, project := newTeamProject(t, "default-group-by")

		if err := insert(project, team, "Sprint board"); err != nil {
			t.Fatalf("create board: %v", err)
		}

		var groupBy string
		if err := pool.QueryRow(ctx,
			`SELECT group_by FROM board WHERE project_id = $1::uuid`, project,
		).Scan(&groupBy); err != nil {
			t.Fatalf("read group_by: %v", err)
		}
		if groupBy != "status" {
			t.Errorf("group_by = %q, want %q", groupBy, "status")
		}
	})

	// The declined UNIQUE (project_id, name). Two people wanting different
	// arrangements of the same project is the normal case, and nothing marks
	// one board as the project's board.
	t.Run("accepts several boards with the same name in one project", func(t *testing.T) {
		team, project := newTeamProject(t, "duplicate-names")

		for range 2 {
			if err := insert(project, team, "Sprint board"); err != nil {
				t.Fatalf("create board: %v", err)
			}
		}
		if got := countBoards(t, project); got != 2 {
			t.Errorf("project holds %d boards, want 2", got)
		}
	})

	t.Run("deleting a project deletes its boards", func(t *testing.T) {
		team, project := newTeamProject(t, "project-cascade")

		if err := insert(project, team, "Sprint board"); err != nil {
			t.Fatalf("create board: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete project: %v", err)
		}
		if got := countBoards(t, project); got != 0 {
			t.Errorf("%d boards survived their project, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the board", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bd-full-chain")
		team := newTeam(t, pool, workspace, "Team bd-full-chain")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := insert(project, team, "Sprint board"); err != nil {
			t.Fatalf("create board: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countBoards(t, project); got != 0 {
			t.Errorf("%d boards survived their workspace, want 0", got)
		}
	})

	// The consequence of the composite reference, and the second table to
	// introduce it after ticket_label. Correct for the same reason: a project
	// changing teams would leave its boards built out of the old team's
	// statuses.
	t.Run("blocks moving a project with boards to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bd-move-project")
		home := newTeam(t, pool, workspace, "Team bd-move-home")
		away := newTeam(t, pool, workspace, "Team bd-move-away")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, home,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := insert(project, home, "Sprint board"); err != nil {
			t.Fatalf("create board: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE project SET team_id = $1::uuid WHERE id = $2::uuid`, away, project)
		wantPgError(t, err, foreignKeyViolation, "moving a project with boards between teams")
	})

	t.Run("project_id leads an index", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1 FROM pg_index i
                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'board'::regclass AND a.attname = 'project_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on board leads with project_id, so listing a " +
				"project's boards and the project-side CASCADE both scan the table")
		}
	})
}

package migrate

import (
	"context"
	"testing"
)

// The constraints 0026_board_column_status.sql claims. The first subtest is
// the one the whole three-table shape exists for: a single column holding
// several statuses, which a status_id column on board_column would have made
// impossible.
//
// The rest hold the last hop of the team chain still - a board must never
// render columns made of another team's statuses - and the delete direction,
// where "no status is sacred" meets "deleting a status must not delete the
// column".
func TestBoardColumnStatusConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// "bcs-" namespaced per file - see schema_test.go.
	newTeamBoard := func(t *testing.T, label string) (team, board string) {
		t.Helper()

		team = newTeam(t, pool, newWorkspace(t, pool, "bcs-"+label), "Team bcs-"+label)

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, 'Sprint board') RETURNING id::text`, project, team,
		).Scan(&board); err != nil {
			t.Fatalf("create board: %v", err)
		}

		return team, board
	}
	newColumn := func(t *testing.T, boardID, teamID, name string, position int) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO board_column (board_id, team_id, name, position)
             VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
			boardID, teamID, name, position,
		).Scan(&id); err != nil {
			t.Fatalf("create column %s: %v", name, err)
		}

		return id
	}
	newStatus := func(t *testing.T, teamID, name, category string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO status (team_id, name, color, position, category)
             VALUES ($1::uuid, $2, '#cccccc', 0, $3) RETURNING id::text`, teamID, name, category,
		).Scan(&id); err != nil {
			t.Fatalf("create status %s: %v", name, err)
		}

		return id
	}
	mapStatus := func(columnID, statusID, teamID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO board_column_status (board_column_id, status_id, team_id)
             VALUES ($1::uuid, $2::uuid, $3::uuid)`, columnID, statusID, teamID)
		return err
	}
	countInColumn := func(t *testing.T, columnID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM board_column_status WHERE board_column_id = $1::uuid`, columnID,
		).Scan(&n); err != nil {
			t.Fatalf("count statuses in the column: %v", err)
		}

		return n
	}
	columnExists := func(t *testing.T, columnID string) bool {
		t.Helper()

		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM board_column WHERE id = $1::uuid)`, columnID,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the column: %v", err)
		}

		return alive
	}
	leadsAnIndex := func(t *testing.T, column string) bool {
		t.Helper()

		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1 FROM pg_index i
                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'board_column_status'::regclass AND a.attname = $1
             )`, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}

		return leads
	}

	// The reason this is a table and not a status_id column on board_column.
	// A "Done" column mapping Done, Won't Do and Duplicate is the ordinary
	// case, and one status per column would have ruled it out permanently.
	t.Run("accepts one column holding several statuses", func(t *testing.T) {
		team, board := newTeamBoard(t, "many-statuses")
		done := newColumn(t, board, team, "Done", 2)

		for _, name := range []string{"Done", "Won't Do", "Duplicate"} {
			if err := mapStatus(done, newStatus(t, team, name, "done"), team); err != nil {
				t.Fatalf("map %s: %v", name, err)
			}
		}
		if got := countInColumn(t, done); got != 3 {
			t.Errorf("column holds %d statuses, want 3", got)
		}
	})

	// And the other direction: one status may appear in columns of two
	// different boards, since two boards are two arrangements of one project.
	t.Run("accepts one status in columns of two boards", func(t *testing.T) {
		team, first := newTeamBoard(t, "two-boards")

		var project, second string
		if err := pool.QueryRow(ctx,
			`SELECT project_id::text FROM board WHERE id = $1::uuid`, first,
		).Scan(&project); err != nil {
			t.Fatalf("find the project: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, 'Triage board') RETURNING id::text`, project, team,
		).Scan(&second); err != nil {
			t.Fatalf("create the second board: %v", err)
		}

		status := newStatus(t, team, "In progress", "in_progress")
		for _, board := range []string{first, second} {
			column := newColumn(t, board, team, "Doing", 1)
			if err := mapStatus(column, status, team); err != nil {
				t.Fatalf("map the status on board %s: %v", board, err)
			}
		}
	})

	t.Run("rejects a column or a status that does not exist", func(t *testing.T) {
		team, board := newTeamBoard(t, "orphans")
		column := newColumn(t, board, team, "To do", 0)
		status := newStatus(t, team, "To do", "not_started")

		err := mapStatus("00000000-0000-0000-0000-000000000000", status, team)
		wantPgError(t, err, foreignKeyViolation, "a column that does not exist")

		err = mapStatus(column, "00000000-0000-0000-0000-000000000000", team)
		wantPgError(t, err, foreignKeyViolation, "a status that does not exist")
	})

	// The last hop of the chain, and the reason 0025 carried team_id down
	// three tables. Both values of team_id are tried; there is no third.
	t.Run("rejects a status belonging to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bcs-cross-team")
		home := newTeam(t, pool, workspace, "Team bcs-cross-home")
		away := newTeam(t, pool, workspace, "Team bcs-cross-away")

		var project, board string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, home,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, 'Sprint board') RETURNING id::text`, project, home,
		).Scan(&board); err != nil {
			t.Fatalf("create board: %v", err)
		}

		column := newColumn(t, board, home, "To do", 0)
		foreign := newStatus(t, away, "To do", "not_started")

		// The column's team satisfies the column hop and breaks the status
		// hop; the status's team does the reverse.
		wantPgError(t, mapStatus(column, foreign, home), foreignKeyViolation,
			"a foreign team's status, with the column's team")
		wantPgError(t, mapStatus(column, foreign, away), foreignKeyViolation,
			"a foreign team's status, with the status's team")
	})

	t.Run("rejects a null column, status or team", func(t *testing.T) {
		team, board := newTeamBoard(t, "nulls")
		column := newColumn(t, board, team, "To do", 0)
		status := newStatus(t, team, "To do", "not_started")

		for _, c := range []struct {
			what  string
			query string
			args  []any
		}{
			{"a null board_column_id", `INSERT INTO board_column_status (board_column_id, status_id, team_id)
                VALUES (NULL, $1::uuid, $2::uuid)`, []any{status, team}},
			{"a null status_id", `INSERT INTO board_column_status (board_column_id, status_id, team_id)
                VALUES ($1::uuid, NULL, $2::uuid)`, []any{column, team}},
			{"a null team_id", `INSERT INTO board_column_status (board_column_id, status_id, team_id)
                VALUES ($1::uuid, $2::uuid, NULL)`, []any{column, status}},
		} {
			_, err := pool.Exec(ctx, c.query, c.args...)
			wantPgError(t, err, notNullViolation, c.what)
		}
	})

	t.Run("rejects the same status twice in one column", func(t *testing.T) {
		team, board := newTeamBoard(t, "duplicate-pair")
		column := newColumn(t, board, team, "Done", 2)
		status := newStatus(t, team, "Done", "done")

		if err := mapStatus(column, status, team); err != nil {
			t.Fatalf("first mapping: %v", err)
		}

		err := mapStatus(column, status, team)
		wantPgError(t, err, uniqueViolation, "the same status twice in one column")
	})

	// Master spec 3.4: no status is sacred. Deleting one must not be blocked
	// by a board holding it, and must not take the column with it - an empty
	// column renders empty rather than disappearing.
	t.Run("deleting a status removes it from columns without deleting them", func(t *testing.T) {
		team, board := newTeamBoard(t, "status-cascade")
		column := newColumn(t, board, team, "Done", 2)
		status := newStatus(t, team, "Won't Do", "done")

		if err := mapStatus(column, status, team); err != nil {
			t.Fatalf("map: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM status WHERE id = $1::uuid`, status); err != nil {
			t.Fatalf("delete the status: %v", err)
		}

		if got := countInColumn(t, column); got != 0 {
			t.Errorf("%d mappings survived their status, want 0", got)
		}
		if !columnExists(t, column) {
			t.Error("deleting a status deleted the board column holding it")
		}
	})

	// ...and the mirror. A status belongs to the team, not to the column.
	t.Run("deleting a column removes its mappings without deleting the status", func(t *testing.T) {
		team, board := newTeamBoard(t, "column-cascade")
		column := newColumn(t, board, team, "Done", 2)
		status := newStatus(t, team, "Done", "done")

		if err := mapStatus(column, status, team); err != nil {
			t.Fatalf("map: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM board_column WHERE id = $1::uuid`, column); err != nil {
			t.Fatalf("delete the column: %v", err)
		}

		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM status WHERE id = $1::uuid)`, status,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the status: %v", err)
		}
		if !alive {
			t.Error("deleting a board column deleted the team's status")
		}
	})

	t.Run("deleting a board removes its columns and their mappings", func(t *testing.T) {
		team, board := newTeamBoard(t, "board-cascade")
		column := newColumn(t, board, team, "Done", 2)

		if err := mapStatus(column, newStatus(t, team, "Done", "done"), team); err != nil {
			t.Fatalf("map: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM board WHERE id = $1::uuid`, board); err != nil {
			t.Fatalf("delete the board: %v", err)
		}
		if got := countInColumn(t, column); got != 0 {
			t.Errorf("%d mappings survived their board, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades down both legs to the mapping", func(t *testing.T) {
		team, board := newTeamBoard(t, "full-chain")
		column := newColumn(t, board, team, "Done", 2)

		if err := mapStatus(column, newStatus(t, team, "Done", "done"), team); err != nil {
			t.Fatalf("map: %v", err)
		}

		var workspace string
		if err := pool.QueryRow(ctx,
			`SELECT workspace_id::text FROM team WHERE id = $1::uuid`, team,
		).Scan(&workspace); err != nil {
			t.Fatalf("find the workspace: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countInColumn(t, column); got != 0 {
			t.Errorf("%d mappings survived their workspace, want 0", got)
		}
	})

	// The consequence of ON UPDATE NO ACTION on the status side: a status
	// cannot be moved to another team while a board column holds it, since
	// the move would leave a board rendering a status its team does not own.
	t.Run("blocks moving a mapped status to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bcs-move-status")
		home := newTeam(t, pool, workspace, "Team bcs-move-home")
		away := newTeam(t, pool, workspace, "Team bcs-move-away")

		var project, board string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, home,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, 'Sprint board') RETURNING id::text`, project, home,
		).Scan(&board); err != nil {
			t.Fatalf("create board: %v", err)
		}

		column := newColumn(t, board, home, "To do", 0)
		status := newStatus(t, home, "To do", "not_started")
		if err := mapStatus(column, status, home); err != nil {
			t.Fatalf("map: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE status SET team_id = $1::uuid WHERE id = $2::uuid`, away, status)
		wantPgError(t, err, foreignKeyViolation, "moving a mapped status between teams")
	})

	// The status-side CASCADE's index, and the reverse lookup a renderer does
	// when placing a ticket into a column.
	t.Run("status_id leads an index", func(t *testing.T) {
		if !leadsAnIndex(t, "status_id") {
			t.Error("no index on board_column_status leads with status_id, so " +
				"deleting one status scans every mapping in the instance")
		}
	})

	// Deliberately no separate index on board_column_id: the composite
	// primary key is a btree led by it. This asserts the PK column order,
	// which is what makes the omission safe.
	t.Run("board_column_id leads an index, via the primary key", func(t *testing.T) {
		if !leadsAnIndex(t, "board_column_id") {
			t.Error("no index on board_column_status leads with board_column_id, " +
				"so the primary key no longer covers the column-side foreign key")
		}
	})
}

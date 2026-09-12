package migrate

import (
	"context"
	"testing"
)

// The constraints 0025_board.sql claims for board_column. The middle hop of
// the team chain lives here, and so does the deliberate absence of a
// uniqueness constraint on position - which is what keeps reordering a plain
// UPDATE.
func TestBoardColumnConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// "bc-" namespaced per file - see schema_test.go.
	newBoard := func(t *testing.T, label string) (team, board string) {
		t.Helper()

		team = newTeam(t, pool, newWorkspace(t, pool, "bc-"+label), "Team bc-"+label)

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
	insert := func(boardID, teamID, name string, position int) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO board_column (board_id, team_id, name, position)
             VALUES ($1::uuid, $2::uuid, $3, $4)`, boardID, teamID, name, position)
		return err
	}
	countColumns := func(t *testing.T, boardID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM board_column WHERE board_id = $1::uuid`, boardID,
		).Scan(&n); err != nil {
			t.Fatalf("count columns: %v", err)
		}

		return n
	}

	t.Run("rejects a column whose board does not exist", func(t *testing.T) {
		team, _ := newBoard(t, "no-such-board")

		err := insert("00000000-0000-0000-0000-000000000000", team, "To do", 0)
		wantPgError(t, err, foreignKeyViolation, "a column with no board")
	})

	// The middle hop on its own: a real board and a real team that do not
	// belong together.
	t.Run("rejects a board and a team that are not parent and child", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "bc-mismatched")
		home := newTeam(t, pool, workspace, "Team bc-mismatch-home")
		other := newTeam(t, pool, workspace, "Team bc-mismatch-other")

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

		err := insert(board, other, "To do", 0)
		wantPgError(t, err, foreignKeyViolation, "a column naming a team that does not own its board")
	})

	t.Run("rejects a null board, team, name or position", func(t *testing.T) {
		team, board := newBoard(t, "null-columns")

		for _, c := range []struct {
			what  string
			query string
			args  []any
		}{
			{"a null board_id", `INSERT INTO board_column (board_id, team_id, name, position)
                VALUES (NULL, $1::uuid, 'To do', 0)`, []any{team}},
			{"a null team_id", `INSERT INTO board_column (board_id, team_id, name, position)
                VALUES ($1::uuid, NULL, 'To do', 0)`, []any{board}},
			{"a null name", `INSERT INTO board_column (board_id, team_id, name, position)
                VALUES ($1::uuid, $2::uuid, NULL, 0)`, []any{board, team}},
			{"a null position", `INSERT INTO board_column (board_id, team_id, name, position)
                VALUES ($1::uuid, $2::uuid, 'To do', NULL)`, []any{board, team}},
		} {
			_, err := pool.Exec(ctx, c.query, c.args...)
			wantPgError(t, err, notNullViolation, c.what)
		}
	})

	// Master spec 3.5 calls column visibility a per-view setting; this is the
	// board's own default arrangement, and it is NOT NULL so nothing has to
	// decide what a null visibility means.
	t.Run("defaults is_visible to true", func(t *testing.T) {
		team, board := newBoard(t, "default-visible")

		if err := insert(board, team, "To do", 0); err != nil {
			t.Fatalf("create column: %v", err)
		}

		var visible *bool
		if err := pool.QueryRow(ctx,
			`SELECT is_visible FROM board_column WHERE board_id = $1::uuid`, board,
		).Scan(&visible); err != nil {
			t.Fatalf("read is_visible: %v", err)
		}
		if visible == nil {
			t.Fatal("is_visible came back NULL, want true")
		}
		if !*visible {
			t.Error("is_visible defaulted to false, want true")
		}
	})

	// A column's label is not a status name - that is the point of a board -
	// so nothing constrains it against status, and two columns may share one.
	t.Run("accepts two columns with the same name on one board", func(t *testing.T) {
		team, board := newBoard(t, "duplicate-names")

		for i := range 2 {
			if err := insert(board, team, "In review", i); err != nil {
				t.Fatalf("create column: %v", err)
			}
		}
		if got := countColumns(t, board); got != 2 {
			t.Errorf("board holds %d columns, want 2", got)
		}
	})

	// position is deliberately not unique per board, matching status.position
	// and aspect_type_field.position.
	t.Run("accepts two columns at the same position", func(t *testing.T) {
		team, board := newBoard(t, "duplicate-position")

		for _, name := range []string{"To do", "Backlog"} {
			if err := insert(board, team, name, 0); err != nil {
				t.Fatalf("create column %s: %v", name, err)
			}
		}
		if got := countColumns(t, board); got != 2 {
			t.Errorf("board holds %d columns, want 2", got)
		}
	})

	// ...and the reason that matters: a two-update swap has to work without a
	// temporary value or a deferred constraint. A UNIQUE (board_id, position)
	// would fail on the first of these two statements.
	t.Run("allows reordering as two plain updates", func(t *testing.T) {
		team, board := newBoard(t, "reorder")

		for i, name := range []string{"To do", "Doing"} {
			if err := insert(board, team, name, i); err != nil {
				t.Fatalf("create column %s: %v", name, err)
			}
		}

		if _, err := pool.Exec(ctx,
			`UPDATE board_column SET position = 1 WHERE board_id = $1::uuid AND name = 'To do'`, board,
		); err != nil {
			t.Fatalf("first half of the swap: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE board_column SET position = 0 WHERE board_id = $1::uuid AND name = 'Doing'`, board,
		); err != nil {
			t.Fatalf("second half of the swap: %v", err)
		}

		var first string
		if err := pool.QueryRow(ctx,
			`SELECT name FROM board_column WHERE board_id = $1::uuid
              ORDER BY position, name LIMIT 1`, board,
		).Scan(&first); err != nil {
			t.Fatalf("read the new order: %v", err)
		}
		if first != "Doing" {
			t.Errorf("first column after the swap is %q, want %q", first, "Doing")
		}
	})

	t.Run("deleting a board deletes its columns", func(t *testing.T) {
		team, board := newBoard(t, "board-cascade")

		if err := insert(board, team, "To do", 0); err != nil {
			t.Fatalf("create column: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM board WHERE id = $1::uuid`, board); err != nil {
			t.Fatalf("delete board: %v", err)
		}
		if got := countColumns(t, board); got != 0 {
			t.Errorf("%d columns survived their board, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the column", func(t *testing.T) {
		team, board := newBoard(t, "full-chain")

		if err := insert(board, team, "To do", 0); err != nil {
			t.Fatalf("create column: %v", err)
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
		if got := countColumns(t, board); got != 0 {
			t.Errorf("%d columns survived their workspace, want 0", got)
		}
	})

	t.Run("board_id leads an index", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1 FROM pg_index i
                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'board_column'::regclass AND a.attname = 'board_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on board_column leads with board_id, so every " +
				"board load and the board-side CASCADE scan the table")
		}
	})
}

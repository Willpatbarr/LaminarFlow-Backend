package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
)

// 0047 (LAM-134) removes a status mapped into two columns of one board, keeping the
// column with the lowest position, and leaves other boards and single mappings alone.
func TestBoardStatusOneColumnMigration(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	raw, err := migrations.FS.ReadFile("0047_board_status_one_column.sql")
	if err != nil {
		t.Fatalf("read 0047: %v", err)
	}
	up := strings.SplitN(strings.SplitN(string(raw), "-- +migrate Down", 2)[0], "-- +migrate Up", 2)[1]

	team := newTeam(t, pool, newWorkspace(t, pool, "b1c-ws"), "Team b1c")
	var project, status1, status2 string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO project (team_id, name) VALUES ($1::uuid, 'P') RETURNING id::text`, team).Scan(&project))
	for name, dst := range map[string]*string{"a": &status1, "b": &status2} {
		must(pool.QueryRow(ctx, `INSERT INTO status (team_id, name, color, position, category)
			VALUES ($1::uuid, $2, '#cccccc', 0, 'not_started') RETURNING id::text`, team, name).Scan(dst))
	}
	newBoard := func() string {
		var id string
		must(pool.QueryRow(ctx, `INSERT INTO board (project_id, team_id, name) VALUES ($1::uuid, $2::uuid, 'B') RETURNING id::text`, project, team).Scan(&id))
		return id
	}
	newCol := func(board string, pos int) string {
		var id string
		must(pool.QueryRow(ctx, `INSERT INTO board_column (board_id, team_id, name, position) VALUES ($1::uuid, $2::uuid, 'c', $3) RETURNING id::text`, board, team, pos).Scan(&id))
		return id
	}
	mapIt := func(col, st string) {
		_, err := pool.Exec(ctx, `INSERT INTO board_column_status (board_column_id, status_id, team_id) VALUES ($1::uuid, $2::uuid, $3::uuid)`, col, st, team)
		must(err)
	}

	b1, b2 := newBoard(), newBoard()
	first, second, third := newCol(b1, 1), newCol(b1, 2), newCol(b1, 3)
	otherBoard := newCol(b2, 1)
	mapIt(first, status1)  // duplicate: kept
	mapIt(third, status1)  // duplicate: removed
	mapIt(second, status2) // single: kept
	mapIt(second, status1) // duplicate: removed
	mapIt(otherBoard, status1)

	_, err = pool.Exec(ctx, up)
	must(err)

	has := func(col, st string) bool {
		var n int
		must(pool.QueryRow(ctx, `SELECT count(*) FROM board_column_status WHERE board_column_id = $1::uuid AND status_id = $2::uuid`, col, st).Scan(&n))
		return n == 1
	}
	if !has(first, status1) || has(second, status1) || has(third, status1) {
		t.Errorf("status a on board 1 must stay only in the lowest-position column")
	}
	if !has(second, status2) || !has(otherBoard, status1) {
		t.Errorf("single mappings and other boards must be untouched")
	}
}

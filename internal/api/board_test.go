package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// LAM-85. The board API end to end: boards, columns, the statuses a column holds, the
// one-column-per-status rule the schema leaves open, and per-board visibility.

type columnWire struct {
	ID        string   `json:"id"`
	BoardID   string   `json:"board_id"`
	Name      string   `json:"name"`
	Position  int      `json:"position"`
	IsVisible bool     `json:"is_visible"`
	StatusIDs []string `json:"status_ids"`
}

type boardWire struct {
	ID        string       `json:"id"`
	ProjectID string       `json:"project_id"`
	Name      string       `json:"name"`
	GroupBy   string       `json:"group_by"`
	Columns   []columnWire `json:"columns"`
}

func (f feed) boards(t *testing.T) []boardWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/boards/list", map[string]any{"project_id": f.project.ID}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("list boards: status %d - %.300s", rec.Code, rec.Body)
	}
	var page pageWire[boardWire]
	decode(t, rec, &page)
	if page.HasMore || page.NextCursor != nil {
		t.Errorf("board list %+v, want a single page", page)
	}

	return page.Items
}

func (f feed) board(t *testing.T, id string) boardWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/boards/"+id, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get board: status %d - %.300s", rec.Code, rec.Body)
	}
	var b boardWire
	decode(t, rec, &b)

	return b
}

// newBoard makes a second board on the project and returns it with its seeded columns.
func (f feed) newBoard(t *testing.T, name string) boardWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/projects/"+f.project.ID+"/boards", map[string]any{"name": name}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("create board: status %d - %.300s", rec.Code, rec.Body)
	}
	var b boardWire
	decode(t, rec, &b)

	return b
}

func wantProblem(t *testing.T, code int, body []byte, status int, errCode, location string) {
	t.Helper()

	if code != status {
		t.Fatalf("status %d, want %d - %.300s", code, status, body)
	}
	var p problemWire
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("problem %s: %v", body, err)
	}
	if p.Code != errCode {
		t.Errorf("code %q, want %q - %.300s", p.Code, errCode, body)
	}
	if location != "" && (len(p.Errors) == 0 || p.Errors[0].Location != location) {
		t.Errorf("errors %+v, want location %q", p.Errors, location)
	}
}

func TestEveryBoardOperationRequiresACaller(t *testing.T) {
	f := newFeed(t)
	id := "00000000-0000-0000-0000-000000000001"

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, V1 + "/projects/" + id + "/boards"},
		{http.MethodPost, V1 + "/boards/list"},
		{http.MethodGet, V1 + "/boards/" + id},
		{http.MethodPut, V1 + "/boards/" + id},
		{http.MethodDelete, V1 + "/boards/" + id},
		{http.MethodPost, V1 + "/boards/" + id + "/columns"},
		{http.MethodPut, V1 + "/boards/" + id + "/columns/" + id},
		{http.MethodPut, V1 + "/boards/" + id + "/columns/" + id + "/statuses"},
		{http.MethodDelete, V1 + "/boards/" + id + "/columns/" + id},
		{http.MethodPost, V1 + "/boards/" + id + "/columns/reorder"},
		{http.MethodPost, V1 + "/tickets/" + id + "/move"},
	} {
		if rec := call(t, f.mux, c.method, c.path, map[string]any{}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a caller: status %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

func TestAProjectsDefaultBoardIsListedWithItsColumns(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses

	boards := f.boards(t)
	if len(boards) != 1 {
		t.Fatalf("%d boards, want the one seeded with the project", len(boards))
	}
	b := f.board(t, boards[0].ID)
	if b.ProjectID != f.project.ID || b.GroupBy != "status" || len(b.Columns) != len(statuses) {
		t.Fatalf("board %+v, want a column per status of %d", b, len(statuses))
	}
	for i, c := range b.Columns {
		if !c.IsVisible || c.BoardID != b.ID || !slices.Equal(c.StatusIDs, []string{statuses[i].ID}) {
			t.Errorf("column %d = %+v, want visible and holding %s", i, c, statuses[i].ID)
		}
	}
}

func TestABoardCanBeCreatedRenamedAndDeleted(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))

	made := f.newBoard(t, "Sprint view")
	if made.Name != "Sprint view" || made.GroupBy != "status" || len(made.Columns) != 3 {
		t.Errorf("created %+v, want a status board seeded with the team's three columns", made)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.created" || ev.BoardID != made.ID || ev.ProjectID != f.project.ID {
		t.Errorf("event %+v, want board.created for %s", ev, made.ID)
	}
	if n := len(f.boards(t)); n != 2 {
		t.Errorf("%d boards, want the default and the new one", n)
	}

	rec := call(t, f.mux, http.MethodPut, V1+"/boards/"+made.ID, map[string]any{"name": "Triage"}, f.willC)
	var renamed boardWire
	decode(t, rec, &renamed)
	if rec.Code != http.StatusOK || renamed.Name != "Triage" || len(renamed.Columns) != 3 {
		t.Fatalf("rename: status %d, %+v", rec.Code, renamed)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.updated" || ev.BoardID != made.ID {
		t.Errorf("event %+v, want board.updated", ev)
	}

	// A ticket in a status the board's columns held survives the board.
	tk := f.withStatus(t, "Survivor", f.team(t).Statuses[0].ID)
	readEvent(t, sarah)

	if rec := call(t, f.mux, http.MethodDelete, V1+"/boards/"+made.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d - %.300s", rec.Code, rec.Body)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.deleted" || ev.BoardID != made.ID {
		t.Errorf("event %+v, want board.deleted", ev)
	}
	if n := count(t, f.pool, `SELECT count(*) FROM board_column WHERE board_id = $1::uuid`, made.ID); n != 0 {
		t.Errorf("%d columns left after the board went", n)
	}
	if got := f.ticket(t, tk.ID); got.Version != 1 {
		t.Errorf("the ticket was touched by the board going: %+v", got)
	}
	wantProblem(t, call(t, f.mux, http.MethodGet, V1+"/boards/"+made.ID, nil, f.willC).Code,
		call(t, f.mux, http.MethodGet, V1+"/boards/"+made.ID, nil, f.willC).Body.Bytes(), 404, "board_not_found", "")

	// The last board of a project may go too.
	for _, b := range f.boards(t) {
		if rec := call(t, f.mux, http.MethodDelete, V1+"/boards/"+b.ID, nil, f.willC); rec.Code != http.StatusNoContent {
			t.Fatalf("delete last: status %d", rec.Code)
		}
	}
	if n := len(f.boards(t)); n != 0 {
		t.Errorf("%d boards, want none", n)
	}
}

func TestBoardFieldsAreValidated(t *testing.T) {
	f := newFeed(t)
	path := V1 + "/projects/" + f.project.ID + "/boards"

	for name, body := range map[string]map[string]any{
		"no name":     {},
		"empty name":  {"name": ""},
		"blank name":  {"name": "   "},
		"long name":   {"name": string(make([]byte, 101))},
		"bad group":   {"name": "x", "group_by": "assignee"},
		"explicit ok": nil,
	} {
		if body == nil {
			body = map[string]any{"name": "ok", "group_by": "status"}
			if rec := call(t, f.mux, http.MethodPost, path, body, f.willC); rec.Code != http.StatusOK {
				t.Errorf("%s: status %d, want 200", name, rec.Code)
			}

			continue
		}
		if rec := call(t, f.mux, http.MethodPost, path, body, f.willC); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, rec.Code)
		}
	}

	rec := call(t, f.mux, http.MethodPost, path, map[string]any{"name": "   "}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "unprocessable_entity", "body.name")
}

func TestBoardsAreScopedToTheWorkspace(t *testing.T) {
	f := newFeed(t)
	b := f.boards(t)[0]
	col := b.Columns[0]

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, V1 + "/boards/" + b.ID, nil},
		{http.MethodPut, V1 + "/boards/" + b.ID, map[string]any{"name": "x"}},
		{http.MethodDelete, V1 + "/boards/" + b.ID, nil},
		{http.MethodPost, V1 + "/boards/" + b.ID + "/columns", map[string]any{"name": "x"}},
		{http.MethodPut, V1 + "/boards/" + b.ID + "/columns/" + col.ID, map[string]any{"name": "x"}},
		{http.MethodPut, V1 + "/boards/" + b.ID + "/columns/" + col.ID + "/statuses", map[string]any{"status_ids": []string{}}},
		{http.MethodDelete, V1 + "/boards/" + b.ID + "/columns/" + col.ID, nil},
		{http.MethodPost, V1 + "/boards/" + b.ID + "/columns/reorder", map[string]any{"column_ids": []string{col.ID}}},
	} {
		rec := call(t, f.mux, c.method, c.path, c.body, f.carolC)
		wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "board_not_found", "")
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/projects/"+f.project.ID+"/boards", map[string]any{"name": "x"}, f.carolC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "project_not_found", "")
	rec = call(t, f.mux, http.MethodPost, V1+"/boards/list", map[string]any{"project_id": f.project.ID}, f.carolC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "project_not_found", "")

	if n := len(f.board(t, b.ID).Columns); n != 3 {
		t.Errorf("board has %d columns after the outsider's attempts, want 3", n)
	}
}

func TestAColumnCanBeAddedWithStatuses(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	b := f.newBoard(t, "Second")
	readEvent(t, sarah)
	statuses := f.team(t).Statuses

	// Free a status first: the new board starts with every status mapped.
	first := b.Columns[0]
	rec := call(t, f.mux, http.MethodPut, V1+"/boards/"+b.ID+"/columns/"+first.ID+"/statuses",
		map[string]any{"status_ids": []string{}}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap: status %d - %.300s", rec.Code, rec.Body)
	}
	readEvent(t, sarah)

	rec = call(t, f.mux, http.MethodPost, V1+"/boards/"+b.ID+"/columns",
		map[string]any{"name": "Backlog", "status_ids": []string{statuses[0].ID}}, f.willC)
	var col columnWire
	decode(t, rec, &col)
	if rec.Code != http.StatusOK || col.Name != "Backlog" || col.Position != 4 || !col.IsVisible ||
		!slices.Equal(col.StatusIDs, []string{statuses[0].ID}) || col.BoardID != b.ID {
		t.Fatalf("add: status %d, %+v, want Backlog last holding %s", rec.Code, col, statuses[0].ID)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.column.updated" || ev.BoardID != b.ID {
		t.Errorf("event %+v, want board.column.updated", ev)
	}

	// With no statuses at all.
	rec = call(t, f.mux, http.MethodPost, V1+"/boards/"+b.ID+"/columns", map[string]any{"name": "Parking lot"}, f.willC)
	decode(t, rec, &col)
	if rec.Code != http.StatusOK || col.Position != 5 || col.StatusIDs == nil || len(col.StatusIDs) != 0 {
		t.Errorf("add bare: status %d, %+v, want position 5 and an empty list", rec.Code, col)
	}
	readEvent(t, sarah)

	// A status that is not the team's, and one that is held by a column already.
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")
	rec = call(t, f.mux, http.MethodPost, V1+"/boards/"+b.ID+"/columns",
		map[string]any{"name": "Wrong", "status_ids": []string{statuses[1].ID, other.Statuses[0].ID}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "status_not_found", "body.status_ids[1]")

	rec = call(t, f.mux, http.MethodPost, V1+"/boards/"+b.ID+"/columns",
		map[string]any{"name": "Dup", "status_ids": []string{statuses[1].ID}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "status_already_mapped", "body.status_ids[0]")

	rec = call(t, f.mux, http.MethodPost, V1+"/boards/"+b.ID+"/columns",
		map[string]any{"name": "Junk", "status_ids": []string{"nope"}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "status_not_found", "body.status_ids[0]")

	if n := len(f.board(t, b.ID).Columns); n != 5 {
		t.Errorf("board has %d columns, want 5: the refused adds wrote nothing", n)
	}
}

func TestAStatusSitsInOneColumnPerBoard(t *testing.T) {
	f := newFeed(t)
	b := f.boards(t)[0]
	statuses := f.team(t).Statuses
	path := func(c columnWire) string { return V1 + "/boards/" + b.ID + "/columns/" + c.ID + "/statuses" }

	// Column 0 already holds status 0; column 1 asking for it is refused.
	rec := call(t, f.mux, http.MethodPut, path(b.Columns[1]),
		map[string]any{"status_ids": []string{statuses[1].ID, statuses[0].ID}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "status_already_mapped", "body.status_ids")

	// A status of another team.
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")
	rec = call(t, f.mux, http.MethodPut, path(b.Columns[1]),
		map[string]any{"status_ids": []string{other.Statuses[0].ID}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "status_not_found", "body.status_ids")

	if got := f.board(t, b.ID); !slices.Equal(got.Columns[1].StatusIDs, []string{statuses[1].ID}) {
		t.Errorf("column 1 holds %v after refused writes, want only %s", got.Columns[1].StatusIDs, statuses[1].ID)
	}

	// Moving a status: free it, then claim it. A column may hold several statuses.
	call(t, f.mux, http.MethodPut, path(b.Columns[0]), map[string]any{"status_ids": []string{}}, f.willC)
	rec = call(t, f.mux, http.MethodPut, path(b.Columns[1]),
		map[string]any{"status_ids": []string{statuses[0].ID, statuses[1].ID, statuses[0].ID}}, f.willC)
	var col columnWire
	decode(t, rec, &col)
	if rec.Code != http.StatusOK || !slices.Equal(col.StatusIDs, []string{statuses[0].ID, statuses[1].ID}) {
		t.Fatalf("set: status %d, %+v, want both statuses once each, in status order", rec.Code, col)
	}
	if got := f.board(t, b.ID); len(got.Columns[0].StatusIDs) != 0 {
		t.Errorf("column 0 still holds %v", got.Columns[0].StatusIDs)
	}

	// The rule is per board: another board may map the same statuses.
	second := f.newBoard(t, "Second")
	if !slices.Equal(second.Columns[0].StatusIDs, []string{statuses[0].ID}) {
		t.Errorf("second board's first column holds %v, want %s", second.Columns[0].StatusIDs, statuses[0].ID)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/boards/"+b.ID+"/columns/"+"00000000-0000-0000-0000-000000000001"+"/statuses",
		map[string]any{"status_ids": []string{}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "column_not_found", "")

	// A column of another board is not found on this one.
	rec = call(t, f.mux, http.MethodPut, V1+"/boards/"+b.ID+"/columns/"+second.Columns[0].ID+"/statuses",
		map[string]any{"status_ids": []string{}}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "column_not_found", "")
}

func TestColumnVisibilityIsPerBoard(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	a := f.boards(t)[0]
	b := f.newBoard(t, "Second")
	readEvent(t, sarah)

	rec := call(t, f.mux, http.MethodPut, V1+"/boards/"+a.ID+"/columns/"+a.Columns[2].ID,
		map[string]any{"is_visible": false}, f.willC)
	var col columnWire
	decode(t, rec, &col)
	if rec.Code != http.StatusOK || col.IsVisible || col.Name != a.Columns[2].Name || col.Position != a.Columns[2].Position {
		t.Fatalf("hide: status %d, %+v, want hidden with name and position untouched", rec.Code, col)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.column.updated" || ev.BoardID != a.ID {
		t.Errorf("event %+v, want board.column.updated", ev)
	}

	if f.board(t, a.ID).Columns[2].IsVisible {
		t.Errorf("column still visible on the board it was hidden on")
	}
	for i, c := range f.board(t, b.ID).Columns {
		if !c.IsVisible {
			t.Errorf("column %d of the other board was hidden too", i)
		}
	}

	// Rename and show again, each member optional.
	rec = call(t, f.mux, http.MethodPut, V1+"/boards/"+a.ID+"/columns/"+a.Columns[2].ID,
		map[string]any{"name": "Shipped", "is_visible": true}, f.willC)
	decode(t, rec, &col)
	if col.Name != "Shipped" || !col.IsVisible {
		t.Errorf("update %+v, want Shipped and visible", col)
	}
	rec = call(t, f.mux, http.MethodPut, V1+"/boards/"+a.ID+"/columns/"+a.Columns[2].ID, map[string]any{}, f.willC)
	decode(t, rec, &col)
	if rec.Code != http.StatusOK || col.Name != "Shipped" {
		t.Errorf("empty update: status %d, %+v, want it left alone", rec.Code, col)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/boards/"+a.ID+"/columns/"+a.Columns[2].ID, map[string]any{"name": " "}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "unprocessable_entity", "body.name")
}

func TestDeletingAColumnUnmapsItsStatusesAndLeavesTickets(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	b := f.boards(t)[0]
	statuses := f.team(t).Statuses
	tk := f.withStatus(t, "Stays put", statuses[0].ID)
	readEvent(t, sarah)

	rec := call(t, f.mux, http.MethodDelete, V1+"/boards/"+b.ID+"/columns/"+b.Columns[0].ID, nil, f.willC)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete column: status %d - %.300s", rec.Code, rec.Body)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.updated" || ev.BoardID != b.ID {
		t.Errorf("event %+v, want board.updated", ev)
	}

	got := f.board(t, b.ID)
	if len(got.Columns) != 2 {
		t.Fatalf("%d columns, want 2", len(got.Columns))
	}
	if n := count(t, f.pool, `SELECT count(*) FROM board_column_status WHERE status_id = $1::uuid`, statuses[0].ID); n != 0 {
		t.Errorf("%d mappings of the deleted column's status, want 0", n)
	}
	if got := f.ticket(t, tk.ID); got.Version != 1 || got.StatusID == nil || *got.StatusID != statuses[0].ID {
		t.Errorf("the ticket was touched by the column going: %+v", got)
	}
	if n := count(t, f.pool, `SELECT count(*) FROM status WHERE id = $1::uuid`, statuses[0].ID); n != 1 {
		t.Errorf("the status went with its column")
	}

	rec = call(t, f.mux, http.MethodDelete, V1+"/boards/"+b.ID+"/columns/"+b.Columns[0].ID, nil, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "column_not_found", "")
}

func TestColumnsAreReorderedByTheCompleteList(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	b := f.boards(t)[0]
	c := b.Columns
	path := V1 + "/boards/" + b.ID + "/columns/reorder"

	rec := call(t, f.mux, http.MethodPost, path, map[string]any{"column_ids": []string{c[2].ID, c[0].ID, c[1].ID}}, f.willC)
	var got boardWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Columns[0].ID != c[2].ID || got.Columns[1].ID != c[0].ID ||
		got.Columns[2].ID != c[1].ID || got.Columns[0].Position != 1 || got.Columns[2].Position != 3 {
		t.Fatalf("reorder: status %d, %+v", rec.Code, got.Columns)
	}
	if ev := readEvent(t, sarah); ev.Type != "board.updated" || ev.BoardID != b.ID {
		t.Errorf("event %+v, want board.updated", ev)
	}

	for name, ids := range map[string][]string{
		"partial":   {c[0].ID, c[1].ID},
		"duplicate": {c[0].ID, c[0].ID, c[1].ID},
		"foreign":   {c[0].ID, c[1].ID, "00000000-0000-0000-0000-000000000001"},
		"junk":      {c[0].ID, c[1].ID, "nope"},
		"empty":     {},
	} {
		rec := call(t, f.mux, http.MethodPost, path, map[string]any{"column_ids": ids}, f.willC)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, rec.Code)
			continue
		}
		wantProblem(t, rec.Code, rec.Body.Bytes(), 422, "column_set_mismatch", "body.column_ids")
	}

	if again := f.board(t, b.ID); again.Columns[0].ID != c[2].ID {
		t.Errorf("a refused reorder changed the order")
	}
}

// LAM-85, the card order LAM-107 needs.

func (f feed) ticket(t *testing.T, id string) ticketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/tickets/"+id, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get ticket: status %d - %.300s", rec.Code, rec.Body)
	}
	var tk ticketWire
	decode(t, rec, &tk)

	return tk
}

func (f feed) move(t *testing.T, id string, body map[string]any, header ...string) (int, []byte) {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/"+id+"/move", body, f.willC, header...)

	return rec.Code, rec.Body.Bytes()
}

// column returns the titles of the live tickets in a status, in board order.
func (f feed) column(t *testing.T, statusID string) []string {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/list", map[string]any{
		"filter": map[string]any{"op": "and", "conditions": []any{
			map[string]any{"field": "status", "op": "eq", "value": statusID},
		}},
		"sort": []any{map[string]any{"field": "position", "desc": false}},
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tickets: status %d - %.300s", rec.Code, rec.Body)
	}
	var page pageWire[struct {
		Title    string `json:"title"`
		Position int    `json:"position"`
	}]
	decode(t, rec, &page)

	out := make([]string, len(page.Items))
	for i, it := range page.Items {
		out[i] = it.Title
	}

	return out
}

func TestADroppedTicketTakesItsPlaceInTheColumn(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	st := f.team(t).Statuses
	todo, doing := st[0].ID, st[1].ID

	a := f.withStatus(t, "A", todo)
	b := f.withStatus(t, "B", todo)
	c := f.withStatus(t, "C", todo)
	d := f.withStatus(t, "D", doing)
	for i := 0; i < 4; i++ {
		readEvent(t, sarah)
	}
	if !(a.Version == 1) {
		t.Fatalf("version %d, want 1", a.Version)
	}

	if got := f.column(t, todo); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Fatalf("column %v, want creation order", got)
	}

	// No placement: the end of the target column, in another status.
	code, body := f.move(t, a.ID, map[string]any{"status_id": doing})
	var moved struct {
		StatusID string `json:"status_id"`
		Position int    `json:"position"`
		Version  int    `json:"version"`
	}
	_ = json.Unmarshal(body, &moved)
	if code != http.StatusOK || moved.StatusID != doing || moved.Version != 2 {
		t.Fatalf("move to end: status %d - %s", code, body)
	}
	if ev := readEvent(t, sarah); ev.Type != "ticket.updated" || ev.TicketID != a.ID || ev.Version != 2 {
		t.Errorf("event %+v, want ticket.updated at version 2", ev)
	}
	if got := f.column(t, doing); !slices.Equal(got, []string{"D", "A"}) {
		t.Errorf("doing column %v, want D then A", got)
	}

	// Above and below a neighbour.
	if code, body := f.move(t, a.ID, map[string]any{"status_id": todo, "before_id": b.ID}); code != http.StatusOK {
		t.Fatalf("before: status %d - %s", code, body)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"A", "B", "C"}) {
		t.Errorf("after A before B: %v", got)
	}
	if code, body := f.move(t, a.ID, map[string]any{"status_id": todo, "after_id": c.ID}); code != http.StatusOK {
		t.Fatalf("after: status %d - %s", code, body)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"B", "C", "A"}) {
		t.Errorf("after A after C: %v", got)
	}
	if code, body := f.move(t, c.ID, map[string]any{"status_id": todo, "before_id": b.ID}); code != http.StatusOK {
		t.Fatalf("before first: status %d - %s", code, body)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"C", "B", "A"}) {
		t.Errorf("C to the top: %v", got)
	}

	// Within the same column, landing between two neighbours.
	if code, body := f.move(t, a.ID, map[string]any{"status_id": todo, "after_id": c.ID}); code != http.StatusOK {
		t.Fatalf("between: status %d - %s", code, body)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"C", "A", "B"}) {
		t.Errorf("A between C and B: %v", got)
	}

	// Another ticket in another column is not a neighbour of this one.
	code, body = f.move(t, a.ID, map[string]any{"status_id": todo, "before_id": d.ID})
	wantProblem(t, code, body, 422, "neighbour_not_found", "body.before_id")
	code, body = f.move(t, a.ID, map[string]any{"status_id": todo, "after_id": d.ID})
	wantProblem(t, code, body, 422, "neighbour_not_found", "body.after_id")
	code, body = f.move(t, a.ID, map[string]any{"status_id": todo, "after_id": a.ID})
	wantProblem(t, code, body, 422, "neighbour_not_found", "body.after_id")
}

func TestManyDropsIntoOneGapRenumberInsteadOfFailing(t *testing.T) {
	f := newFeed(t)
	todo := f.team(t).Statuses[0].ID

	first := f.withStatus(t, "first", todo)
	last := f.withStatus(t, "last", todo)
	var movers []ticketWire
	for _, name := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9", "m10", "m11", "m12"} {
		movers = append(movers, f.withStatus(t, name, todo))
	}

	// Each lands just after "first", halving the gap above it, so the 1024 between the
	// neighbours runs out after ten drops and a renumber has to happen.
	want := []string{"first"}
	for _, m := range movers {
		if code, body := f.move(t, m.ID, map[string]any{"status_id": todo, "after_id": first.ID}); code != http.StatusOK {
			t.Fatalf("drop %s: status %d - %s", m.Title, code, body)
		}
		want = append([]string{want[0], m.Title}, want[1:]...)
	}
	want = append(want, "last")
	_ = last

	// The movers are in reverse order of the drops, then the rest.
	if got := f.column(t, todo); !slices.Equal(got, want) {
		t.Errorf("column %v, want %v", got, want)
	}
}

func TestAMoveTakesAnExplicitPositionAndRefusesAmbiguity(t *testing.T) {
	f := newFeed(t)
	todo := f.team(t).Statuses[0].ID
	a := f.withStatus(t, "A", todo)
	b := f.withStatus(t, "B", todo)

	code, body := f.move(t, b.ID, map[string]any{"status_id": todo, "position": 1})
	var out struct {
		Position int `json:"position"`
	}
	_ = json.Unmarshal(body, &out)
	if code != http.StatusOK || out.Position != 1 {
		t.Fatalf("position: status %d - %s", code, body)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"B", "A"}) {
		t.Errorf("column %v, want B first", got)
	}

	code, body = f.move(t, a.ID, map[string]any{"status_id": todo, "position": 5, "before_id": b.ID})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("two placements: status %d - %s, want 422", code, body)
	}
	code, _ = f.move(t, a.ID, map[string]any{"status_id": todo, "position": -1})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("negative position: status %d, want 422", code)
	}
	code, _ = f.move(t, a.ID, map[string]any{"position": 3})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("no status: status %d, want 422", code)
	}
}

func TestAMoveChecksTheStatusTheScopeAndTheVersion(t *testing.T) {
	f := newFeed(t)
	todo := f.team(t).Statuses[0].ID
	a := f.withStatus(t, "A", todo)
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")

	code, body := f.move(t, a.ID, map[string]any{"status_id": other.Statuses[0].ID})
	wantProblem(t, code, body, 422, "status_not_found", "body.status_id")

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/"+a.ID+"/move", map[string]any{"status_id": todo}, f.carolC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), 404, "ticket_not_found", "")

	// A stale If-Match conflicts and writes nothing; the current one goes through.
	if code, body := f.move(t, a.ID, map[string]any{"status_id": todo}, "If-Match", `"1"`); code != http.StatusOK {
		t.Fatalf("move at version 1: status %d - %s", code, body)
	}
	code, body = f.move(t, a.ID, map[string]any{"status_id": todo}, "If-Match", `"1"`)
	wantProblem(t, code, body, 409, "ticket_conflict", "")
	if got := f.ticket(t, a.ID); got.Version != 2 {
		t.Errorf("a refused move changed the ticket: %+v", got)
	}

	// An archived ticket cannot be moved.
	call(t, f.mux, http.MethodDelete, V1+"/tickets/"+a.ID, nil, f.willC)
	code, body = f.move(t, a.ID, map[string]any{"status_id": todo})
	wantProblem(t, code, body, 404, "ticket_not_found", "")
}

func TestNewTicketsGoToTheEndAndExistingOnesAreBackfilled(t *testing.T) {
	f := newFeed(t)
	todo := f.team(t).Statuses[0].ID

	for _, title := range []string{"one", "two", "three"} {
		f.withStatus(t, title, todo)
	}
	if got := f.column(t, todo); !slices.Equal(got, []string{"one", "two", "three"}) {
		t.Errorf("column %v, want creation order", got)
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/list", map[string]any{
		"sort": []any{map[string]any{"field": "position", "desc": false}},
	}, f.willC)
	var page pageWire[struct {
		Position int `json:"position"`
	}]
	decode(t, rec, &page)
	var positions []int
	for _, it := range page.Items {
		positions = append(positions, it.Position)
	}
	if !slices.Equal(positions, []int{1024, 2048, 3072}) {
		t.Errorf("positions %v, want 1024 apart from the first", positions)
	}
}

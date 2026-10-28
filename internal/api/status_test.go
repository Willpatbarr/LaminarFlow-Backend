package api

import (
	"net/http"
	"strings"
	"testing"
)

// LAM-83. The status editor end to end: rename, add, reorder, and the delete rules.

func (f feed) statusPath(suffix string) string {
	return V1 + "/teams/" + f.project.TeamID + "/statuses" + suffix
}

func (f feed) team(t *testing.T) teamWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/teams/"+f.project.TeamID, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get team: status %d - %.300s", rec.Code, rec.Body)
	}
	var out teamWire
	decode(t, rec, &out)

	return out
}

func (f feed) withStatus(t *testing.T, title, statusID string) ticketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": f.project.ID, "title": title, "description": "", "status_id": statusID, "assignee_id": nil,
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("create ticket: status %d - %.300s", rec.Code, rec.Body)
	}
	var out ticketWire
	decode(t, rec, &out)

	return out
}

func TestEveryStatusOperationRequiresACaller(t *testing.T) {
	f := newFeed(t)
	id := f.team(t).Statuses[0].ID

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, f.statusPath("")},
		{http.MethodPut, f.statusPath("/" + id)},
		{http.MethodDelete, f.statusPath("/" + id)},
		{http.MethodPost, f.statusPath("/reorder")},
	} {
		if rec := call(t, f.mux, c.method, c.path, map[string]any{}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a caller: status %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

func TestAStatusCanBeAddedRenamedAndRecoloured(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))

	rec := call(t, f.mux, http.MethodPost, f.statusPath(""),
		map[string]any{"name": "Review", "color": "#fff", "category": "in_progress"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d - %.300s", rec.Code, rec.Body)
	}
	var made statusWire
	decode(t, rec, &made)
	if made.Name != "Review" || made.Position != 4 || made.Category != "in_progress" {
		t.Errorf("created %+v, want Review at position 4 (last)", made)
	}
	if ev := readEvent(t, sarah); ev.Type != "status.created" || ev.StatusID != made.ID || ev.TeamID != f.project.TeamID {
		t.Errorf("event %+v, want status.created for %s", ev, made.ID)
	}

	// An explicit position is stored as given, even a duplicate.
	rec = call(t, f.mux, http.MethodPost, f.statusPath(""),
		map[string]any{"name": "Triage", "color": "#000", "category": "not_started", "position": 1}, f.willC)
	var triage statusWire
	decode(t, rec, &triage)
	if triage.Position != 1 {
		t.Errorf("position %d, want 1", triage.Position)
	}
	readEvent(t, sarah)

	// Rename only: the other members are untouched, and the id is stable.
	rec = call(t, f.mux, http.MethodPut, f.statusPath("/"+made.ID), map[string]any{"name": "In Review"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d - %.300s", rec.Code, rec.Body)
	}
	var renamed statusWire
	decode(t, rec, &renamed)
	if renamed.ID != made.ID || renamed.Name != "In Review" || renamed.Color != "#fff" || renamed.Category != "in_progress" {
		t.Errorf("renamed %+v from %+v", renamed, made)
	}
	if ev := readEvent(t, sarah); ev.Type != "status.updated" || ev.StatusID != made.ID {
		t.Errorf("event %+v, want status.updated", ev)
	}

	if n := len(f.team(t).Statuses); n != 5 {
		t.Errorf("team has %d statuses, want 5", n)
	}
}

func TestStatusFieldsAreValidated(t *testing.T) {
	f := newFeed(t)
	id := f.team(t).Statuses[0].ID
	ok := map[string]any{"name": "n", "color": "c", "category": "done"}

	with := func(k string, v any) map[string]any {
		b := map[string]any{}
		for kk, vv := range ok {
			b[kk] = vv
		}
		b[k] = v

		return b
	}

	for field, body := range map[string]map[string]any{
		"body.name":     with("name", "   "),
		"body.color":    with("color", ""),
		"body.category": with("category", "doing"),
	} {
		refused(t, call(t, f.mux, http.MethodPost, f.statusPath(""), body, f.willC), field)
	}

	refused(t, call(t, f.mux, http.MethodPut, f.statusPath("/"+id), map[string]any{"name": strings.Repeat("a", 101)}, f.willC), "body.name")
	refused(t, call(t, f.mux, http.MethodPut, f.statusPath("/"+id), map[string]any{"category": "nope"}, f.willC), "body.category")
	refused(t, call(t, f.mux, http.MethodPut, f.statusPath("/"+id), map[string]any{"name": "  "}, f.willC), "body.name")
}

func TestAnUnknownOrForeignStatusIsA404(t *testing.T) {
	f := newFeed(t)
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")
	foreign := other.Statuses[0].ID

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, id := range []string{foreign, "8b1c0000-0000-4000-8000-000000000000"} {
			rec := call(t, f.mux, method, f.statusPath("/"+id), map[string]any{}, f.willC)
			var p problemWire
			decode(t, rec, &p)
			if rec.Code != http.StatusNotFound || p.Code != CodeStatusNotFound {
				t.Errorf("%s %s: status %d code %q, want 404 status_not_found", method, id, rec.Code, p.Code)
			}
		}
	}

	// Carol is in another workspace: the team itself is not hers.
	rec := call(t, f.mux, http.MethodPost, f.statusPath(""),
		map[string]any{"name": "x", "color": "c", "category": "done"}, f.carolC)
	if rec.Code != http.StatusNotFound {
		t.Errorf("outsider create: status %d, want 404", rec.Code)
	}
}

func TestReorderWritesTheWholeOrderAndRefusesAWrongSet(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	st := f.team(t).Statuses
	a, b, c := st[0].ID, st[1].ID, st[2].ID

	rec := call(t, f.mux, http.MethodPost, f.statusPath("/reorder"), map[string]any{"status_ids": []string{c, a, b}}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder: status %d - %.300s", rec.Code, rec.Body)
	}
	var got teamWire
	decode(t, rec, &got)
	if len(got.Statuses) != 3 || got.Statuses[0].ID != c || got.Statuses[1].ID != a || got.Statuses[2].ID != b ||
		got.Statuses[0].Position != 1 || got.Statuses[2].Position != 3 {
		t.Errorf("order after reorder: %+v", got.Statuses)
	}
	// All three moved, so three events.
	for i := 0; i < 3; i++ {
		if ev := readEvent(t, sarah); ev.Type != "status.updated" {
			t.Errorf("event %d = %s, want status.updated", i, ev.Type)
		}
	}

	for name, ids := range map[string][]string{
		"partial":   {a, b},
		"duplicate": {a, a, b},
		"foreign":   {a, b, "8b1c0000-0000-4000-8000-000000000000"},
		"not uuids": {a, b, "x"},
	} {
		p := refused(t, call(t, f.mux, http.MethodPost, f.statusPath("/reorder"), map[string]any{"status_ids": ids}, f.willC), "body.status_ids")
		if p.Code != CodeStatusSetWrong {
			t.Errorf("%s: code %q", name, p.Code)
		}
	}
}

func TestDeleteMovesTheTicketsAndTheBoardMapping(t *testing.T) {
	f := newFeed(t)
	st := f.team(t).Statuses
	doomed, keep := st[0].ID, st[1].ID

	one := f.withStatus(t, "one", doomed)
	two := f.withStatus(t, "two", doomed)
	elsewhere := f.withStatus(t, "elsewhere", st[2].ID)
	archived := f.withStatus(t, "gone", doomed)
	call(t, f.mux, http.MethodDelete, V1+"/tickets/"+archived.ID, nil, f.willC)

	sarah := f.open(t, f.cookie(f.sarahC))

	rec := call(t, f.mux, http.MethodDelete, f.statusPath("/"+doomed+"?move_to="+keep), nil, f.willC)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d - %.300s", rec.Code, rec.Body)
	}

	// status.deleted, then one ticket.updated per moved ticket (the archived one too).
	if ev := readEvent(t, sarah); ev.Type != "status.deleted" || ev.StatusID != doomed {
		t.Errorf("first event %+v, want status.deleted", ev)
	}
	moved := map[string]bool{}
	for i := 0; i < 3; i++ {
		ev := readEvent(t, sarah)
		// 2 for a live ticket (create, move); the archived one has also been archived.
		if ev.Type != "ticket.updated" || ev.Version < 2 || ev.Ticket["status_id"] != keep {
			t.Errorf("moved ticket event %+v", ev)
		}
		moved[ev.TicketID] = true
	}
	if !moved[one.ID] || !moved[two.ID] || !moved[archived.ID] || moved[elsewhere.ID] {
		t.Errorf("moved set %v", moved)
	}

	for _, id := range []string{one.ID, two.ID} {
		var got ticketWire
		decode(t, call(t, f.mux, http.MethodGet, V1+"/tickets/"+id, nil, f.willC), &got)
		if got.StatusID == nil || *got.StatusID != keep || got.Version != 2 {
			t.Errorf("ticket %s = %+v, want status %s at version 2", id, got, keep)
		}
	}

	if n := len(f.team(t).Statuses); n != 2 {
		t.Errorf("%d statuses left, want 2", n)
	}

	// The board column that showed the deleted status now shows keep; none was lost.
	var cols int
	if err := f.pool.QueryRow(testCtx(t),
		`SELECT count(*) FROM board_column_status WHERE status_id = $1::uuid`, keep).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 2 {
		t.Errorf("%d board mappings for the kept status, want 2 (its own column, and the deleted status's)", cols)
	}
	var orphans int
	if err := f.pool.QueryRow(testCtx(t),
		`SELECT count(*) FROM board_column_status WHERE status_id = $1::uuid`, doomed).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("mappings for the deleted status: %d (%v)", orphans, err)
	}
}

func TestDeleteWithoutMoveToIsRefusedWhileTicketsUseTheStatus(t *testing.T) {
	f := newFeed(t)
	st := f.team(t).Statuses
	f.withStatus(t, "a", st[0].ID)
	f.withStatus(t, "b", st[0].ID)
	f.withStatus(t, "c", st[0].ID)

	rec := call(t, f.mux, http.MethodDelete, f.statusPath("/"+st[0].ID), nil, f.willC)
	p := refused(t, rec, "query.move_to")
	if p.Code != CodeStatusInUse {
		t.Errorf("code %q", p.Code)
	}
	if !strings.Contains(rec.Body.String(), "3 tickets use this status; pass move_to") {
		t.Errorf("detail does not name the count: %.300s", rec.Body)
	}
	if n := len(f.team(t).Statuses); n != 3 {
		t.Errorf("%d statuses, want 3 - the refused delete must change nothing", n)
	}

	// An unused status deletes without move_to.
	if rec := call(t, f.mux, http.MethodDelete, f.statusPath("/"+st[1].ID), nil, f.willC); rec.Code != http.StatusNoContent {
		t.Errorf("unused: status %d - %.300s", rec.Code, rec.Body)
	}
}

func TestMoveToMustBeAnotherStatusOfTheTeam(t *testing.T) {
	f := newFeed(t)
	st := f.team(t).Statuses
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")

	for name, moveTo := range map[string]string{
		"itself":  st[0].ID,
		"foreign": other.Statuses[0].ID,
		"unknown": "8b1c0000-0000-4000-8000-000000000000",
	} {
		p := refused(t, call(t, f.mux, http.MethodDelete, f.statusPath("/"+st[0].ID+"?move_to="+moveTo), nil, f.willC), "query.move_to")
		if p.Code != CodeStatusNotFound {
			t.Errorf("%s: code %q", name, p.Code)
		}
	}
}

func TestTheLastStatusGoesOnlyWhenNoTicketsExist(t *testing.T) {
	f := newFeed(t)
	st := f.team(t).Statuses

	for _, s := range st[1:] {
		if rec := call(t, f.mux, http.MethodDelete, f.statusPath("/"+s.ID), nil, f.willC); rec.Code != http.StatusNoContent {
			t.Fatalf("delete: status %d - %.300s", rec.Code, rec.Body)
		}
	}

	// A ticket with no status still counts: it exists.
	f.create(t, f.willC, "statusless")

	p := refused(t, call(t, f.mux, http.MethodDelete, f.statusPath("/"+st[0].ID), nil, f.willC), "query.move_to")
	if p.Code != CodeStatusInUse {
		t.Errorf("code %q, want status_in_use", p.Code)
	}

	if len(f.team(t).Statuses) != 1 {
		t.Errorf("the last status must survive a refused delete")
	}
}

func TestTheLastStatusDeletesFromATeamWithNoTickets(t *testing.T) {
	f := newFeed(t)

	for _, s := range f.team(t).Statuses {
		if rec := call(t, f.mux, http.MethodDelete, f.statusPath("/"+s.ID), nil, f.willC); rec.Code != http.StatusNoContent {
			t.Fatalf("delete %s: status %d - %.300s", s.ID, rec.Code, rec.Body)
		}
	}
	if n := len(f.team(t).Statuses); n != 0 {
		t.Errorf("%d statuses, want none", n)
	}

	// And a team can start over.
	rec := call(t, f.mux, http.MethodPost, f.statusPath(""), map[string]any{"name": "Only", "color": "c", "category": "not_started"}, f.willC)
	var st statusWire
	decode(t, rec, &st)
	if rec.Code != http.StatusOK || st.Position != 1 {
		t.Errorf("recreate: status %d %+v", rec.Code, st)
	}
}

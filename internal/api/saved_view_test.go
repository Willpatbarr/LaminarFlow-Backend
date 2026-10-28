package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// LAM-86. The saved view API end to end: create, get, list, update and delete, who may
// see and change what, and the config checks.

type viewWire struct {
	ID             string         `json:"id"`
	TeamID         string         `json:"team_id"`
	ProjectID      *string        `json:"project_id"`
	BoardID        *string        `json:"board_id"`
	OwnerAccountID *string        `json:"owner_account_id"`
	Name           string         `json:"name"`
	Layout         string         `json:"layout"`
	Config         map[string]any `json:"config"`
	IsShared       bool           `json:"is_shared"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
}

type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// view creates a view as cookie and returns it, failing the test on anything but a 200.
func (f feed) view(t *testing.T, cookie string, extra map[string]any) viewWire {
	t.Helper()

	body := map[string]any{"team_id": f.project.TeamID, "name": "v", "layout": "list"}
	for k, v := range extra {
		body[k] = v
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/saved-views", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("create view: status %d - %.400s", rec.Code, rec.Body)
	}
	var out viewWire
	decode(t, rec, &out)

	return out
}

// refused expects a failure with the given status and code, and returns it.
func svRefused(t *testing.T, _ *httptest.ResponseRecorder, body []byte, status int, code string) problem {
	t.Helper()

	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if p.Status != status || p.Code != code {
		t.Fatalf("got %d %q (%.300s), want %d %q", p.Status, p.Code, body, status, code)
	}

	return p
}

func (f feed) viewNames(t *testing.T, cookie string, extra map[string]any) []string {
	t.Helper()

	body := map[string]any{"team_id": f.project.TeamID}
	for k, v := range extra {
		body[k] = v
	}
	rec := call(t, f.mux, http.MethodPost, V1+"/saved-views/list", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list views: status %d - %.300s", rec.Code, rec.Body)
	}
	var page pageWire[viewWire]
	decode(t, rec, &page)

	names := []string{}
	for _, v := range page.Items {
		names = append(names, v.Name)
	}

	return names
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestEverySavedViewOperationRequiresACaller(t *testing.T) {
	f := newFeed(t)
	id := "00000000-0000-0000-0000-000000000001"

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/saved-views"}, {http.MethodPost, "/saved-views/list"},
		{http.MethodGet, "/saved-views/" + id}, {http.MethodPut, "/saved-views/" + id},
		{http.MethodDelete, "/saved-views/" + id},
	} {
		if rec := call(t, f.mux, c.method, V1+c.path, map[string]any{}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a caller: status %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

func TestSavedViewCreateGetUpdateDelete(t *testing.T) {
	f := newFeed(t)

	cfg := map[string]any{
		"filter":  map[string]any{"op": "and", "conditions": []any{map[string]any{"field": "title", "op": "contains", "value": "bug"}}},
		"group":   "status",
		"sort":    []any{map[string]any{"field": "updated_at", "desc": true}},
		"columns": []any{"title", "status", "assignee"},
	}
	made := f.view(t, f.willC, map[string]any{"name": "Bugs", "config": cfg, "project_id": f.project.ID})

	if made.OwnerAccountID == nil || *made.OwnerAccountID != f.will.AccountID || made.IsShared ||
		made.Layout != "list" || made.BoardID != nil || made.ProjectID == nil || *made.ProjectID != f.project.ID {
		t.Fatalf("created %+v, want a private list view of will's, scoped to the project", made)
	}
	if got, _ := json.Marshal(made.Config); string(got) == "{}" || made.Config["group"] != "status" {
		t.Fatalf("config %s did not round trip", got)
	}

	rec := call(t, f.mux, http.MethodGet, V1+"/saved-views/"+made.ID, nil, f.willC)
	var got viewWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ID != made.ID || got.Name != "Bugs" || len(got.Config["columns"].([]any)) != 3 {
		t.Fatalf("get: status %d, %+v", rec.Code, got)
	}

	// An omitted member is left alone; a config replaces the old one whole.
	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+made.ID,
		map[string]any{"name": "Open bugs", "is_shared": true, "config": map[string]any{"group": "assignee"}}, f.willC)
	var upd viewWire
	decode(t, rec, &upd)
	if rec.Code != http.StatusOK || upd.Name != "Open bugs" || !upd.IsShared || upd.Layout != "list" ||
		upd.Config["group"] != "assignee" || upd.Config["filter"] != nil || upd.Config["columns"] != nil {
		t.Fatalf("update: status %d, %+v", rec.Code, upd)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+made.ID, map[string]any{"name": "Renamed"}, f.willC)
	decode(t, rec, &upd)
	if upd.Name != "Renamed" || !upd.IsShared || upd.Config["group"] != "assignee" {
		t.Errorf("a rename alone changed more: %+v", upd)
	}

	if rec := call(t, f.mux, http.MethodDelete, V1+"/saved-views/"+made.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d - %.300s", rec.Code, rec.Body)
	}
	rec = call(t, f.mux, http.MethodGet, V1+"/saved-views/"+made.ID, nil, f.willC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusNotFound, "saved_view_not_found")
}

func TestPrivateViewsAreTheirOwnersAndSharedViewsAreTheTeams(t *testing.T) {
	f := newFeed(t)

	mine := f.view(t, f.willC, map[string]any{"name": "Will private"})
	shared := f.view(t, f.willC, map[string]any{"name": "Will shared", "is_shared": true})
	f.view(t, f.sarahC, map[string]any{"name": "Sarah private"})

	// The team's seeded view is there too, shared.
	if got, want := f.viewNames(t, f.willC, nil), []string{"All tickets", "Will private", "Will shared"}; !equalNames(got, want) {
		t.Errorf("will sees %v, want %v", got, want)
	}
	if got, want := f.viewNames(t, f.sarahC, nil), []string{"All tickets", "Sarah private", "Will shared"}; !equalNames(got, want) {
		t.Errorf("sarah sees %v, want %v", got, want)
	}

	rec := call(t, f.mux, http.MethodGet, V1+"/saved-views/"+mine.ID, nil, f.sarahC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusNotFound, "saved_view_not_found")
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec = call(t, f.mux, method, V1+"/saved-views/"+mine.ID, map[string]any{"name": "x"}, f.sarahC)
		svRefused(t, rec, rec.Body.Bytes(), http.StatusNotFound, "saved_view_not_found")
	}

	if rec := call(t, f.mux, http.MethodGet, V1+"/saved-views/"+shared.ID, nil, f.sarahC); rec.Code != http.StatusOK {
		t.Errorf("sarah reading a shared view: status %d", rec.Code)
	}

	// Another workspace sees none of it.
	rec = call(t, f.mux, http.MethodGet, V1+"/saved-views/"+shared.ID, nil, f.carolC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusNotFound, "saved_view_not_found")
	rec = call(t, f.mux, http.MethodPost, V1+"/saved-views/list", map[string]any{"team_id": f.project.TeamID}, f.carolC)
	p := svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "team_not_found")
	if p.Errors[0].Location != "body.team_id" {
		t.Errorf("location %q, want body.team_id", p.Errors[0].Location)
	}
	rec = call(t, f.mux, http.MethodPost, V1+"/saved-views",
		map[string]any{"team_id": f.project.TeamID, "name": "x", "layout": "list"}, f.carolC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "team_not_found")
}

func TestOnlyTheOwnerChangesASharedView(t *testing.T) {
	f := newFeed(t)
	shared := f.view(t, f.willC, map[string]any{"name": "Will shared", "is_shared": true})

	rec := call(t, f.mux, http.MethodPut, V1+"/saved-views/"+shared.ID, map[string]any{"name": "Mine now"}, f.sarahC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusForbidden, "saved_view_not_owner")
	rec = call(t, f.mux, http.MethodDelete, V1+"/saved-views/"+shared.ID, nil, f.sarahC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusForbidden, "saved_view_not_owner")

	var got viewWire
	decode(t, call(t, f.mux, http.MethodGet, V1+"/saved-views/"+shared.ID, nil, f.willC), &got)
	if got.Name != "Will shared" {
		t.Errorf("a refused write changed the view: %+v", got)
	}

	// The seeded team view has no owner, so any member may change it - but not hide it.
	var seeded string
	if err := f.pool.QueryRow(testCtx(t),
		`SELECT id::text FROM saved_view WHERE team_id = $1::uuid AND owner_account_id IS NULL`,
		f.project.TeamID).Scan(&seeded); err != nil {
		t.Fatalf("find the seeded view: %v", err)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+seeded, map[string]any{"is_shared": false}, f.sarahC)
	p := svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "unprocessable_entity")
	if p.Errors[0].Location != "body.is_shared" {
		t.Errorf("location %q, want body.is_shared", p.Errors[0].Location)
	}
	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+seeded, map[string]any{"name": "Everything"}, f.sarahC)
	if rec.Code != http.StatusOK {
		t.Fatalf("renaming the team's own view: status %d - %.300s", rec.Code, rec.Body)
	}
	if rec := call(t, f.mux, http.MethodDelete, V1+"/saved-views/"+seeded, nil, f.sarahC); rec.Code != http.StatusNoContent {
		t.Errorf("deleting the team's own view: status %d", rec.Code)
	}
}

func TestSavedViewConfigThatDoesNotCompileIsRefusedOnTheField(t *testing.T) {
	f := newFeed(t)
	ok := f.view(t, f.willC, map[string]any{"name": "ok"})

	cond := func(field, op string, value any) map[string]any {
		return map[string]any{"filter": map[string]any{"op": "and", "conditions": []any{
			map[string]any{"field": field, "op": op, "value": value}}}}
	}

	for _, c := range []struct {
		name, location string
		config         map[string]any
	}{
		{"unknown filter field", "body.config.filter.conditions[0].field", cond("nope", "eq", "x")},
		{"operator wrong for the field", "body.config.filter.conditions[0].op", cond("created_at", "contains", "x")},
		{"bad uuid", "body.config.filter.conditions[0].value", cond("status", "eq", "not-a-uuid")},
		{"unknown sort field", "body.config.sort[0].field", map[string]any{"sort": []any{map[string]any{"field": "nope", "desc": false}}}},
		{"unsortable sort field", "body.config.sort[0].field", map[string]any{"sort": []any{map[string]any{"field": "label", "desc": false}}}},
		{"unknown group", "body.config.group", map[string]any{"group": "nope"}},
		{"multi-valued group", "body.config.group", map[string]any{"group": "label"}},
		{"unknown column", "body.config.columns[1]", map[string]any{"columns": []any{"title", "nope"}}},
		{"repeated column", "body.config.columns[1]", map[string]any{"columns": []any{"title", "title"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, req := range []struct {
				method, path string
				body         map[string]any
			}{
				{http.MethodPost, "/saved-views", map[string]any{"team_id": f.project.TeamID, "name": "x", "layout": "list", "config": c.config}},
				{http.MethodPut, "/saved-views/" + ok.ID, map[string]any{"config": c.config}},
			} {
				rec := call(t, f.mux, req.method, V1+req.path, req.body, f.willC)
				p := svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "filter_invalid")
				if len(p.Errors) == 0 || p.Errors[0].Location != c.location {
					t.Errorf("%s: errors %+v, want first at %s", req.method, p.Errors, c.location)
				}
			}
		})
	}

	// Every fault is reported, not the first.
	rec := call(t, f.mux, http.MethodPost, V1+"/saved-views", map[string]any{
		"team_id": f.project.TeamID, "name": "x", "layout": "list",
		"config": map[string]any{"group": "nope", "columns": []any{"nope"}},
	}, f.willC)
	if p := svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "filter_invalid"); len(p.Errors) != 2 {
		t.Errorf("errors %+v, want both faults", p.Errors)
	}

	// A member the config does not have is refused rather than stored.
	rec = call(t, f.mux, http.MethodPost, V1+"/saved-views", map[string]any{
		"team_id": f.project.TeamID, "name": "x", "layout": "list", "config": map[string]any{"sorts": []any{}},
	}, f.willC)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown config member: status %d, want 422", rec.Code)
	}

	// Nothing above was written.
	if got, want := f.viewNames(t, f.willC, nil), []string{"All tickets", "ok"}; !equalNames(got, want) {
		t.Errorf("views %v after refused writes, want %v", got, want)
	}
}

func TestSavedViewLayoutAndBoardRules(t *testing.T) {
	f := newFeed(t)
	board := f.newBoard(t, "Sprint board").ID
	post := func(extra map[string]any) (*http.Response, problem, []byte) {
		body := map[string]any{"team_id": f.project.TeamID, "name": "x"}
		for k, v := range extra {
			body[k] = v
		}
		rec := call(t, f.mux, http.MethodPost, V1+"/saved-views", body, f.willC)
		var p problem
		_ = json.Unmarshal(rec.Body.Bytes(), &p)

		return rec.Result(), p, rec.Body.Bytes()
	}

	for _, c := range []struct {
		name     string
		body     map[string]any
		code     string
		location string
	}{
		{"board view without a board", map[string]any{"layout": "board"}, "unprocessable_entity", "body.board_id"},
		{"list view with a board", map[string]any{"layout": "list", "board_id": board}, "unprocessable_entity", "body.board_id"},
		{"unknown board", map[string]any{"layout": "board", "board_id": "00000000-0000-0000-0000-000000000001"}, "board_not_found", "body.board_id"},
		{"unknown project", map[string]any{"layout": "list", "project_id": "00000000-0000-0000-0000-000000000001"}, "project_not_found", "body.project_id"},
		{"unknown layout", map[string]any{"layout": "grid"}, "unprocessable_entity", "body.layout"},
		{"blank name", map[string]any{"layout": "list", "name": "   "}, "unprocessable_entity", "body.name"},
	} {
		resp, p, raw := post(c.body)
		if resp.StatusCode != http.StatusUnprocessableEntity || p.Code != c.code || len(p.Errors) == 0 || p.Errors[0].Location != c.location {
			t.Errorf("%s: %d %.300s, want 422 %s at %s", c.name, resp.StatusCode, raw, c.code, c.location)
		}
	}

	// A board in another team is no board of this one.
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")
	otherProject := createProject(t, f.mux, f.willC, other.ID, "Elsewhere")
	rec := call(t, f.mux, http.MethodPost, V1+"/saved-views", map[string]any{
		"team_id": f.project.TeamID, "name": "x", "layout": "list", "project_id": otherProject.ID}, f.willC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "project_not_found")

	bv := f.view(t, f.willC, map[string]any{"layout": "board", "board_id": board, "project_id": f.project.ID, "name": "Board view"})
	if bv.BoardID == nil || *bv.BoardID != board || bv.Layout != "board" {
		t.Fatalf("board view %+v", bv)
	}

	// A list view cannot become a board view - the body names no board.
	lv := f.view(t, f.willC, map[string]any{"name": "List view"})
	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+lv.ID, map[string]any{"layout": "board"}, f.willC)
	p := svRefused(t, rec, rec.Body.Bytes(), http.StatusUnprocessableEntity, "unprocessable_entity")
	if p.Errors[0].Location != "body.layout" {
		t.Errorf("location %q, want body.layout", p.Errors[0].Location)
	}

	// A board view becomes a list view, and loses its board.
	rec = call(t, f.mux, http.MethodPut, V1+"/saved-views/"+bv.ID, map[string]any{"layout": "list"}, f.willC)
	var got viewWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Layout != "list" || got.BoardID != nil {
		t.Errorf("board to list: status %d, %+v", rec.Code, got)
	}
}

func TestSavedViewListScopesToProjectAndPages(t *testing.T) {
	f := newFeed(t)
	other := createProject(t, f.mux, f.willC, f.project.TeamID, "Second")

	f.view(t, f.willC, map[string]any{"name": "a team-wide", "is_shared": true})
	f.view(t, f.willC, map[string]any{"name": "b core", "is_shared": true, "project_id": f.project.ID})
	f.view(t, f.willC, map[string]any{"name": "c second", "is_shared": true, "project_id": other.ID})

	if got, want := f.viewNames(t, f.willC, map[string]any{"project_id": f.project.ID}), []string{"All tickets", "a team-wide", "b core"}; !equalNames(got, want) {
		t.Errorf("project view list %v, want %v", got, want)
	}
	if got, want := f.viewNames(t, f.willC, nil), []string{"All tickets", "a team-wide", "b core", "c second"}; !equalNames(got, want) {
		t.Errorf("team view list %v, want %v", got, want)
	}

	var names []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		body := map[string]any{"team_id": f.project.TeamID, "limit": 3}
		if cursor != "" {
			body["cursor"] = cursor
		}
		rec := call(t, f.mux, http.MethodPost, V1+"/saved-views/list", body, f.willC)
		var page pageWire[viewWire]
		decode(t, rec, &page)
		for _, v := range page.Items {
			names = append(names, v.Name)
		}
		if !page.HasMore {
			if page.NextCursor != nil || pages != 1 {
				t.Errorf("last page %+v after %d pages", page, pages)
			}
			break
		}
		cursor = *page.NextCursor
	}
	if want := []string{"All tickets", "a team-wide", "b core", "c second"}; !equalNames(names, want) {
		t.Errorf("paged %v, want %v", names, want)
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/saved-views/list", map[string]any{"team_id": f.project.TeamID, "cursor": "!!"}, f.willC)
	svRefused(t, rec, rec.Body.Bytes(), http.StatusBadRequest, "bad_cursor")
}

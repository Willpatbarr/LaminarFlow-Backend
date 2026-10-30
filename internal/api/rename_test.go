package api

import (
	"net/http"
	"strings"
	"testing"
)

// LAM-149. Renaming a team and a project: validation, scoping, and the change feed.

func TestEveryRenameRequiresACaller(t *testing.T) {
	f := newFeed(t)

	for _, path := range []string{V1 + "/teams/" + f.project.TeamID, V1 + "/projects/" + f.project.ID} {
		if rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": "x"}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("PUT %s without a caller: status %d, want 401", path, rec.Code)
		}
	}
}

func TestATeamCanBeRenamedAndTheWorkspaceIsTold(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	path := V1 + "/teams/" + f.project.TeamID

	rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": "Platform"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d - %.300s", rec.Code, rec.Body)
	}
	var got teamWire
	decode(t, rec, &got)
	if got.ID != f.project.TeamID || got.Name != "Platform" || len(got.Statuses) == 0 {
		t.Errorf("renamed %+v, want the same team, named Platform, with its statuses", got)
	}
	if ev := readEvent(t, sarah); ev.Type != "team.updated" || ev.TeamID != f.project.TeamID || ev.ActorID != f.will.AccountID {
		t.Errorf("event %+v, want team.updated for the team", ev)
	}
	if f.team(t).Name != "Platform" {
		t.Error("the new name was not kept")
	}

	// The same name again is not a collision with itself.
	if rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": "Platform"}, f.willC); rec.Code != http.StatusOK {
		t.Errorf("rename to its own name: status %d, want 200", rec.Code)
	}
}

func TestATeamNameMustBeValidAndUnique(t *testing.T) {
	f := newFeed(t)
	path := V1 + "/teams/" + f.project.TeamID

	for _, name := range []string{"", "   ", strings.Repeat("a", 101)} {
		if rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": name}, f.willC); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("name %q: status %d, want 422", name, rec.Code)
		}
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/teams",
		map[string]any{"workspace_id": f.team(t).WorkspaceID, "name": "Taken"}, f.willC)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create second team: status %d - %.300s", rec.Code, rec.Body)
	}
	rec = call(t, f.mux, http.MethodPut, path, map[string]any{"name": "Taken"}, f.willC)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), CodeTeamNameTaken) {
		t.Errorf("duplicate name: status %d - %.200s, want 409 %s", rec.Code, rec.Body, CodeTeamNameTaken)
	}
}

func TestAProjectCanBeRenamedAndTheWorkspaceIsTold(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	path := V1 + "/projects/" + f.project.ID

	rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": "Core"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d - %.300s", rec.Code, rec.Body)
	}
	var got projectWire
	decode(t, rec, &got)
	if got.ID != f.project.ID || got.Name != "Core" || got.TeamID != f.project.TeamID {
		t.Errorf("renamed %+v, want the same project, named Core", got)
	}
	if ev := readEvent(t, sarah); ev.Type != "project.updated" || ev.ProjectID != f.project.ID || ev.TeamID != f.project.TeamID {
		t.Errorf("event %+v, want project.updated for the project", ev)
	}

	for _, name := range []string{"", "   ", strings.Repeat("a", 101)} {
		if rec := call(t, f.mux, http.MethodPut, path, map[string]any{"name": name}, f.willC); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("name %q: status %d, want 422", name, rec.Code)
		}
	}
}

func TestAnOutsiderCannotRenameATeamOrProject(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPut, V1+"/teams/"+f.project.TeamID, map[string]any{"name": "Mine"}, f.carolC)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), CodeTeamNotFound) {
		t.Errorf("team: status %d - %.200s, want 404 %s", rec.Code, rec.Body, CodeTeamNotFound)
	}
	rec = call(t, f.mux, http.MethodPut, V1+"/projects/"+f.project.ID, map[string]any{"name": "Mine"}, f.carolC)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), CodeProjectNotFound) {
		t.Errorf("project: status %d - %.200s, want 404 %s", rec.Code, rec.Body, CodeProjectNotFound)
	}
	if f.team(t).Name == "Mine" {
		t.Error("an outsider renamed the team")
	}
}

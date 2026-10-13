package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

// LAM-64: the reads a client needs to hydrate. Registered through the list machinery, so
// what these pin is the part that is per resource - the path, the vocabulary, the item
// shape - and that nothing about the shared half was bypassed.

var hydrationLists = []struct {
	name   string
	path   string
	absent string // a field the vocabulary must not offer
	has    []string
}{
	{"teams", V1 + "/teams/list", "statuses", []string{"`name`", "`created_at`", "`updated_at`"}},
	{"projects", V1 + "/projects/list", "board", []string{"`name`", "`team`", "`created_at`", "`updated_at`"}},
}

func TestTheHydrationListsAreRegisteredAndRequireACaller(t *testing.T) {
	ops := operations(t)

	for _, l := range hydrationLists {
		op := ops["POST "+l.path]
		if op == nil {
			t.Errorf("%s list is not registered at POST %s", l.name, l.path)
			continue
		}
		if required, _ := op.Metadata[RequireAuth].(bool); !required {
			t.Errorf("%s list is not marked RequireAuth, so it is public", l.name)
		}
	}
}

// Generated from the whitelist, so each list publishes its own vocabulary and not
// ticket's. A copy-pasted description is the failure this is for.
func TestEachHydrationListPublishesItsOwnVocabulary(t *testing.T) {
	ops := operations(t)

	for _, l := range hydrationLists {
		op := ops["POST "+l.path]
		if op == nil {
			t.Fatalf("%s list is not registered", l.name)
		}

		for _, want := range l.has {
			if !strings.Contains(op.Description, want) {
				t.Errorf("%s list does not publish %s", l.name, want)
			}
		}
		for _, notWant := range []string{"`" + l.absent + "`", "`title`", "`assignee`", "`label`"} {
			if strings.Contains(op.Description, notWant) {
				t.Errorf("%s list offers %s, which its vocabulary does not have", l.name, notWant)
			}
		}
	}
}

func TestTheHydrationListsAreUnreachableWithoutACookie(t *testing.T) {
	mux := testMux(t)

	for _, l := range hydrationLists {
		rec := call(t, mux, http.MethodPost, l.path, map[string]any{}, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s list without a cookie: status %d, want 401", l.name, rec.Code)
		}
	}
}

type statusWire struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Position int    `json:"position"`
	Category string `json:"category"`
}

type teamWire struct {
	ID          string       `json:"id"`
	WorkspaceID string       `json:"workspace_id"`
	Name        string       `json:"name"`
	Statuses    []statusWire `json:"statuses"`
}

type projectWire struct {
	ID     string `json:"id"`
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
}

type pageWire[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

func createTeam(t *testing.T, mux *http.ServeMux, cookie, workspace, name string) teamWire {
	t.Helper()

	rec := call(t, mux, http.MethodPost, V1+"/teams", map[string]any{"workspace_id": workspace, "name": name, "bare": false}, cookie)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create team %s: status %d, body %s", name, rec.Code, rec.Body)
	}

	var out teamWire
	decode(t, rec, &out)

	return out
}

func createProject(t *testing.T, mux *http.ServeMux, cookie, teamID, name string) projectWire {
	t.Helper()

	rec := call(t, mux, http.MethodPost, V1+"/projects", map[string]any{"team_id": teamID, "name": name, "bare": false}, cookie)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create project %s: status %d, body %s", name, rec.Code, rec.Body)
	}

	var out projectWire
	decode(t, rec, &out)

	return out
}

// The whole point of the ticket, end to end: register, create, and hydrate from two
// calls. The statuses a client groups tickets by arrive with their category and order.
func TestAClientCanHydrateTeamsStatusesAndProjectsInTwoCalls(t *testing.T) {
	mux, _ := registrationMux(t, false)
	me, cookie := register(t, mux, "will@example.com")

	platform := createTeam(t, mux, cookie, me.WorkspaceID, "Platform")
	core := createProject(t, mux, cookie, platform.ID, "Core")

	rec := call(t, mux, http.MethodPost, V1+"/teams/list", map[string]any{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("teams/list: status %d, body %s", rec.Code, rec.Body)
	}

	var teams pageWire[teamWire]
	decode(t, rec, &teams)

	if len(teams.Items) != 1 || teams.Items[0].ID != platform.ID || teams.Items[0].WorkspaceID != me.WorkspaceID {
		t.Fatalf("teams = %+v, want the one team in the caller's workspace", teams.Items)
	}
	if len(teams.Items[0].Statuses) != 3 {
		t.Fatalf("got %d statuses, want 3: %+v", len(teams.Items[0].Statuses), teams.Items[0].Statuses)
	}
	for i, st := range teams.Items[0].Statuses {
		if st.Category == "" || st.Position != i+1 || st.ID != platform.Statuses[i].ID {
			t.Errorf("status %d = %+v, want category set, position %d, id %s", i, st, i+1, platform.Statuses[i].ID)
		}
	}
	if teams.NextCursor != nil || teams.HasMore {
		t.Errorf("one team produced next_cursor %v, has_more %v", teams.NextCursor, teams.HasMore)
	}

	rec = call(t, mux, http.MethodPost, V1+"/projects/list", map[string]any{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects/list: status %d, body %s", rec.Code, rec.Body)
	}

	var projects pageWire[projectWire]
	decode(t, rec, &projects)

	if len(projects.Items) != 1 || projects.Items[0] != core {
		t.Errorf("projects = %+v, want %+v", projects.Items, core)
	}
}

// A client must never have to handle null for "no rows" or "no statuses".
func TestAnEmptyHydrationListIsAnEmptyArrayOnTheWire(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	for _, l := range hydrationLists {
		rec := call(t, mux, http.MethodPost, l.path, map[string]any{}, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", l.name, rec.Code, rec.Body)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte(`"items":[]`)) {
			t.Errorf("%s: body %s, want items as []", l.name, rec.Body)
		}
	}
}

// A teammate joins the same workspace (ADR 0004) and hydrates the same teams: scope is
// the workspace, not the account that created the rows.
func TestAWorkspaceMateSeesTheSameTeams(t *testing.T) {
	mux, _ := registrationMux(t, false)
	first, firstCookie := register(t, mux, "first@example.com")
	_, secondCookie := register(t, mux, "second@example.com")

	createTeam(t, mux, firstCookie, first.WorkspaceID, "Platform")

	rec := call(t, mux, http.MethodPost, V1+"/teams/list", map[string]any{}, secondCookie)
	var teams pageWire[teamWire]
	decode(t, rec, &teams)

	if len(teams.Items) != 1 || teams.Items[0].Name != "Platform" {
		t.Errorf("the second member saw %+v", teams.Items)
	}
}

func TestPaginationWorksOverHTTP(t *testing.T) {
	mux, _ := registrationMux(t, false)
	me, cookie := register(t, mux, "will@example.com")

	for _, name := range []string{"A", "B", "C"} {
		createTeam(t, mux, cookie, me.WorkspaceID, name)
	}

	var (
		seen   []string
		cursor string
		pages  int
	)
	for {
		body := map[string]any{"limit": 2}
		if cursor != "" {
			body["cursor"] = cursor
		}

		rec := call(t, mux, http.MethodPost, V1+"/teams/list", body, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", pages+1, rec.Code, rec.Body)
		}

		var page pageWire[teamWire]
		decode(t, rec, &page)
		for _, tm := range page.Items {
			seen = append(seen, tm.Name)
		}
		pages++

		if !page.HasMore {
			if page.NextCursor != nil {
				t.Error("the last page has a next_cursor")
			}
			break
		}
		if page.NextCursor == nil || pages > 5 {
			t.Fatalf("has_more without a usable cursor after %d pages", pages)
		}
		cursor = *page.NextCursor
	}

	if pages != 2 || strings.Join(seen, "") != "ABC" {
		t.Errorf("%d pages visiting %v, want 2 pages visiting A, B, C", pages, seen)
	}
}

// The shared error mapping reaches the new lists: one entry per problem, at its location,
// under the existing code - no new code was needed.
func TestAHydrationFilterFaultIsA422WithTheListCode(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodPost, V1+"/projects/list", map[string]any{
		"filter": map[string]any{
			"op":         "and",
			"conditions": []map[string]any{{"field": "title", "op": "eq", "value": "x"}},
		},
	}, cookie)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}

	e := errorOf(t, rec)
	if e.Code != CodeFilterInvalid || len(e.Errors) != 1 || e.Errors[0].Location != "body.filter.conditions[0].field" {
		t.Errorf("error = %+v", e)
	}
}

func TestAnOversizeHydrationPageIsRejectedNotClamped(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodPost, V1+"/teams/list", map[string]any{"limit": filter.MaxLimit + 1}, cookie)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422, body %s", rec.Code, rec.Body)
	}
}

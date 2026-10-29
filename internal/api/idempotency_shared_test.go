package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// LAM-127. Idempotency-Key on the creates that share the idempotency_key table (0045):
// teams, projects, statuses, epics, sprints, milestones, labels, boards, columns, saved
// views, aspect types, imports and fields. The five older creates keep their own tests.

// sharedCase is one create. body gets a name so a replay can send a different one.
type sharedCase struct {
	op    string // operation id, as in the OpenAPI document
	path  string
	event string // the change-feed event it publishes, "" for none
	body  func(f feed, fx sharedFixtures, name string) map[string]any
}

type sharedFixtures struct{ board, aspect string }

func sharedCases() []sharedCase {
	return []sharedCase{
		{"create-team", "/teams", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"workspace_id": f.will.WorkspaceID, "name": n, "bare": false}
		}},
		{"create-project", "/projects", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"team_id": f.project.TeamID, "name": n, "bare": false}
		}},
		{"create-status", "/teams/{team}/statuses", "status.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"name": n, "color": "#fff", "category": "in_progress"}
		}},
		{"create-epic", "/epics", "epic.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"project_id": f.project.ID, "name": n}
		}},
		{"create-sprint", "/sprints", "sprint.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"project_id": f.project.ID, "name": n}
		}},
		{"create-milestone", "/milestones", "milestone.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"project_id": f.project.ID, "name": n}
		}},
		{"create-label", "/labels", "label.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"team_id": f.project.TeamID, "name": n, "color": "#123456"}
		}},
		{"create-board", "/projects/{project}/boards", "board.created", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"name": n}
		}},
		{"add-column", "/boards/{board}/columns", "board.column.updated", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"name": n}
		}},
		{"create-saved-view", "/saved-views", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"team_id": f.project.TeamID, "name": n, "layout": "list"}
		}},
		{"create-aspect-type", "/aspect-types", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"team_id": f.project.TeamID, "name": n}
		}},
		{"import-aspect-type", "/aspect-types/import", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"team_id": f.project.TeamID, "format_version": 1, "name": n,
				"fields": []map[string]any{{"label": "a", "position": 1}}}
		}},
		{"add-aspect-type-field", "/aspect-types/{aspect}/fields", "", func(f feed, _ sharedFixtures, n string) map[string]any {
			return map[string]any{"label": n}
		}},
	}
}

func (f feed) sharedFixtures(t *testing.T) sharedFixtures {
	t.Helper()

	var boards struct {
		Items []boardWire `json:"items"`
	}
	decode(t, call(t, f.mux, http.MethodPost, V1+"/boards/list", map[string]any{"project_id": f.project.ID}, f.willC), &boards)
	if len(boards.Items) == 0 {
		t.Fatal("the project has no board")
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/aspect-types", map[string]any{"team_id": f.project.TeamID, "name": "Fixture"}, f.willC)
	var at struct {
		ID string `json:"id"`
	}
	decode(t, rec, &at)

	return sharedFixtures{board: boards.Items[0].ID, aspect: at.ID}
}

func (c sharedCase) url(f feed, fx sharedFixtures) string {
	return V1 + strings.NewReplacer("{team}", f.project.TeamID, "{project}", f.project.ID,
		"{board}", fx.board, "{aspect}", fx.aspect).Replace(c.path)
}

func (c sharedCase) post(t *testing.T, f feed, fx sharedFixtures, cookie, key, name string) *httptest.ResponseRecorder {
	t.Helper()

	if key == "" {
		return call(t, f.mux, http.MethodPost, c.url(f, fx), c.body(f, fx, name), cookie)
	}

	return call(t, f.mux, http.MethodPost, c.url(f, fx), c.body(f, fx, name), cookie, "Idempotency-Key", key)
}

func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var out struct {
		ID string `json:"id"`
	}
	decode(t, rec, &out)

	return out.ID
}

func TestEverySharedCreateReplaysOnARepeatedKey(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)

	for _, c := range sharedCases() {
		t.Run(c.op, func(t *testing.T) {
			watcher := f.open(t, f.cookie(f.sarahC))

			first := c.post(t, f, fx, f.willC, "k-"+c.op, "Original")
			second := c.post(t, f, fx, f.willC, "k-"+c.op, "A different name")

			if first.Code != http.StatusOK || second.Code != http.StatusOK {
				t.Fatalf("statuses %d and %d, want 200 for both - %.300s / %.300s", first.Code, second.Code, first.Body, second.Body)
			}
			if got := first.Header().Get("Idempotent-Replayed"); got != "false" {
				t.Errorf("first Idempotent-Replayed = %q, want false", got)
			}
			if got := second.Header().Get("Idempotent-Replayed"); got != "true" {
				t.Errorf("second Idempotent-Replayed = %q, want true", got)
			}
			a, b := idOf(t, first), idOf(t, second)
			if a == "" || a != b {
				t.Errorf("ids %q and %q, want the same", a, b)
			}

			// Frames arrive in order, so everything the two creates published is before the
			// marker's ticket.created. Exactly one of the create's own event, none for a replay.
			marker := f.create(t, f.willC, "marker "+c.op)
			seen := 0
			for {
				ev := readEventAny(t, watcher)
				if ev.Type == "ticket.created" && ev.TicketID == marker.ID {
					break
				}
				if c.event != "" && ev.Type == c.event {
					seen++
				}
			}
			want := 0
			if c.event != "" {
				want = 1
			}
			if seen != want {
				t.Errorf("%d %q events, want %d", seen, c.event, want)
			}
			quiet(t, watcher)
			watcher.Close(websocket.StatusNormalClosure, "")
		})
	}
}

// quiet fails when another frame is waiting after the marker's activity.
func quiet(t *testing.T, c *websocket.Conn) {
	t.Helper()

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var ev eventWire
		if json.Unmarshal(data, &ev) == nil && ev.Type == "activity.created" {
			continue // the marker's own log entry
		}
		t.Errorf("an extra frame arrived: %s", data)
	}
}

func TestSharedCreatesWithoutAKeyAreUnchanged(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)

	for _, c := range sharedCases() {
		a := c.post(t, f, fx, f.willC, "", "one "+c.op)
		b := c.post(t, f, fx, f.willC, "", "two "+c.op)
		if _, present := a.Header()["Idempotent-Replayed"]; present {
			t.Errorf("%s: Idempotent-Replayed present without a key", c.op)
		}
		if a.Code != http.StatusOK || b.Code != http.StatusOK || idOf(t, a) == idOf(t, b) {
			t.Errorf("%s: two plain creates gave %d and %d with ids %q and %q", c.op, a.Code, b.Code, idOf(t, a), idOf(t, b))
		}
	}
}

func TestASharedKeyIsPerAccountAndPerOperation(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)
	epic, milestone := sharedCases()[3], sharedCases()[5]

	will := epic.post(t, f, fx, f.willC, "shared", "Will's")
	sarah := epic.post(t, f, fx, f.sarahC, "shared", "Sarah's")
	if idOf(t, will) == idOf(t, sarah) || sarah.Header().Get("Idempotent-Replayed") != "false" {
		t.Error("one account's key returned another's epic")
	}

	other := milestone.post(t, f, fx, f.willC, "shared", "Same key, other operation")
	if other.Header().Get("Idempotent-Replayed") != "false" || idOf(t, other) == idOf(t, will) {
		t.Errorf("the same key on another operation was a replay (%q)", other.Header().Get("Idempotent-Replayed"))
	}
}

func TestADeletedResourceFreesItsSharedKey(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)
	epic := sharedCases()[3]

	first := epic.post(t, f, fx, f.willC, "k", "One")
	id := idOf(t, first)
	if rec := call(t, f.mux, http.MethodDelete, V1+"/epics/"+id, nil, f.willC); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}

	again := epic.post(t, f, fx, f.willC, "k", "Two")
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "false" || idOf(t, again) == id {
		t.Errorf("after delete: status %d replayed %q id %q (old %q), want a fresh create", again.Code, again.Header().Get("Idempotent-Replayed"), idOf(t, again), id)
	}
}

func TestAReplayReturnsTheCurrentSharedResource(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)
	epic := sharedCases()[3]

	id := idOf(t, epic.post(t, f, fx, f.willC, "k", "Original"))
	if rec := call(t, f.mux, http.MethodPut, V1+"/epics/"+id, map[string]any{"name": "Renamed"}, f.willC); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}

	var got struct {
		Name string `json:"name"`
	}
	decode(t, epic.post(t, f, fx, f.willC, "k", "Original"), &got)
	if got.Name != "Renamed" {
		t.Errorf("replay name = %q, want the current one", got.Name)
	}
}

func TestSixteenConcurrentSharedCreatesWithOneKeyMakeOneResource(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)

	for _, c := range []sharedCase{sharedCases()[3], sharedCases()[2]} {
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			ids = map[string]bool{}
		)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := c.post(t, f, fx, f.willC, "race", "t")
				mu.Lock()
				ids[idOf(t, rec)] = true
				mu.Unlock()
				if rec.Code != http.StatusOK {
					t.Errorf("status %d - %.200s", rec.Code, rec.Body)
				}
			}()
		}
		wg.Wait()

		if len(ids) != 1 {
			t.Errorf("%s: 16 concurrent creates made %d resources, want 1", c.op, len(ids))
		}
	}
}

func TestAFailedSharedCreateLeavesItsKeyFree(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)
	epic := sharedCases()[3]

	bad := call(t, f.mux, http.MethodPost, V1+"/epics", map[string]any{"project_id": "00000000-0000-4000-8000-000000000000", "name": "x"},
		f.willC, "Idempotency-Key", "k")
	if bad.Code != http.StatusNotFound {
		t.Fatalf("create in a missing project: %d", bad.Code)
	}

	ok := epic.post(t, f, fx, f.willC, "k", "Now it works")
	if ok.Code != http.StatusOK || ok.Header().Get("Idempotent-Replayed") != "false" {
		t.Errorf("retry after a failure: status %d replayed %q", ok.Code, ok.Header().Get("Idempotent-Replayed"))
	}
}

func TestASharedKeyMustBeWellFormed(t *testing.T) {
	f := newFeed(t)
	fx := f.sharedFixtures(t)
	epic := sharedCases()[3]

	for _, key := range []string{strings.Repeat("k", 129), "café"} {
		refused(t, epic.post(t, f, fx, f.willC, key, "x"), "header.Idempotency-Key")
	}

	refused(t, call(t, f.mux, http.MethodPost, V1+"/epics", epic.body(f, fx, "x"), f.willC, "Idempotency-Key", ""), "header.Idempotency-Key")
}

func TestTheOpenAPIDocumentsTheKeyOnEverySharedCreate(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	for _, c := range sharedCases() {
		var op = doc.Paths[V1+strings.NewReplacer("{team}", "{id}", "{project}", "{id}", "{board}", "{id}", "{aspect}", "{id}").Replace(c.path)]
		if op == nil || op.Post == nil {
			t.Errorf("%s: no POST in the document", c.op)
			continue
		}

		var header bool
		for _, p := range op.Post.Parameters {
			if p.In == "header" && p.Name == "Idempotency-Key" {
				header = true
				if p.Required || p.Schema == nil || p.Schema.MaxLength == nil || *p.Schema.MaxLength != 128 {
					t.Errorf("%s: Idempotency-Key = %+v, want optional, maxLength 128", c.op, p)
				}
			}
		}
		if !header {
			t.Errorf("%s does not document Idempotency-Key", c.op)
		}
		if ok := op.Post.Responses["200"]; ok == nil || ok.Headers["Idempotent-Replayed"] == nil {
			t.Errorf("%s: the 200 does not document Idempotent-Replayed", c.op)
		}
	}
}

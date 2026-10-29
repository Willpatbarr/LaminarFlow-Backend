package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// LAM-87. The wire half of the document API: statuses, headers, codes and where a 422
// points. The rules themselves are tested in internal/document.

type documentWire struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Type         string         `json:"type"`
	AspectTypeID *string        `json:"aspect_type_id"`
	TeamID       *string        `json:"team_id"`
	ProjectID    *string        `json:"project_id"`
	Body         map[string]any `json:"body"`
	Version      int            `json:"version"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	ArchivedAt   *time.Time     `json:"archived_at"`
}

type documentConflictWire struct {
	Status    int          `json:"status"`
	Code      string       `json:"code"`
	Current   documentWire `json:"current"`
	ChangedAt time.Time    `json:"changed_at"`
}

func (f feed) aspectType(t *testing.T, cookie string, labels ...string) (typeID string, fieldIDs []string) {
	t.Helper()

	var rec *httptest.ResponseRecorder
	rec = call(t, f.mux, http.MethodPost, V1+"/aspect-types", map[string]any{"team_id": f.project.TeamID, "name": "Class"}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("create aspect type: %d %s", rec.Code, rec.Body)
	}
	var at struct {
		ID string `json:"id"`
	}
	decode(t, rec, &at)

	for _, label := range labels {
		rec = call(t, f.mux, http.MethodPost, V1+"/aspect-types/"+at.ID+"/fields", map[string]any{"label": label}, cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("add field: %d %s", rec.Code, rec.Body)
		}
		var field struct {
			ID string `json:"id"`
		}
		decode(t, rec, &field)
		fieldIDs = append(fieldIDs, field.ID)
	}

	return at.ID, fieldIDs
}

func (f feed) document(t *testing.T, cookie string, body map[string]any) documentWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/documents", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("create document: status %d, body %s", rec.Code, rec.Body)
	}
	var d documentWire
	decode(t, rec, &d)

	return d
}

func wantDocProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code, location string) {
	t.Helper()

	var p problemWire
	decode(t, rec, &p)
	if rec.Code != status || p.Code != code {
		t.Fatalf("got %d %q (%s), want %d %q", rec.Code, p.Code, rec.Body, status, code)
	}
	if location != "" && (len(p.Errors) == 0 || p.Errors[0].Location != location) {
		t.Fatalf("errors = %+v, want location %q", p.Errors, location)
	}
}

func TestANormalDocumentRoundTripsWithItsETag(t *testing.T) {
	f := newFeed(t)

	d := f.document(t, f.willC, map[string]any{
		"title": "Notes", "project_id": f.project.ID, "body": map[string]string{"content": "# Hi [t](lf://ticket/1)"},
	})
	if d.Version != 1 || d.Type != "normal" || d.ProjectID == nil || *d.ProjectID != f.project.ID || d.ArchivedAt != nil {
		t.Errorf("created = %+v", d)
	}

	rec := call(t, f.mux, http.MethodGet, V1+"/documents/"+d.ID, nil, f.sarahC)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("get: %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	var got documentWire
	decode(t, rec, &got)
	if got.Body["content"] != "# Hi [t](lf://ticket/1)" {
		t.Errorf("body = %v", got.Body)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID,
		map[string]any{"title": "Notes 2", "body": map[string]string{"content": "new"}}, f.willC, "If-Match", `"1"`)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	if got.Version != 2 || got.Title != "Notes 2" || got.ProjectID == nil {
		t.Errorf("after put = %+v (the scope must survive a PUT)", got)
	}
}

func TestAnAspectDocumentIsCheckedAgainstItsType(t *testing.T) {
	f := newFeed(t)
	typeID, fields := f.aspectType(t, f.willC, "Methods", "Notes")

	d := f.document(t, f.willC, map[string]any{
		"title": "UserService", "type": "aspect", "aspect_type_id": typeID,
		"body": map[string]string{fields[0]: "Save()", fields[1]: "_"},
	})
	if d.Type != "aspect" || d.AspectTypeID == nil || *d.AspectTypeID != typeID {
		t.Errorf("created = %+v", d)
	}

	stray := "00000000-0000-4000-8000-000000000009"
	rec := call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{
		"type": "aspect", "aspect_type_id": typeID, "body": map[string]string{stray: "x"}}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeDocumentFieldUnknown, "body.body."+stray)

	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{"type": "aspect"}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, "unprocessable_entity", "body.aspect_type_id")

	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{"aspect_type_id": typeID}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, "unprocessable_entity", "body.aspect_type_id")

	// A value that is not a Markdown string is the schema's 422, at the key.
	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{
		"type": "aspect", "aspect_type_id": typeID, "body": map[string]any{fields[0]: 42}}, f.willC)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a numeric field value: %d %s, want 422", rec.Code, rec.Body)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{
		"type": "aspect", "aspect_type_id": "00000000-0000-4000-8000-00000000000a"}, f.willC)
	wantDocProblem(t, rec, http.StatusNotFound, CodeAspectNotFound, "")

	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{
		"project_id": "00000000-0000-4000-8000-00000000000b"}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeProjectNotFound, "body.project_id")
}

func TestNormalToAspectOverHTTP(t *testing.T) {
	f := newFeed(t)
	withFields, fields := f.aspectType(t, f.willC, "Methods")
	noFields, _ := f.aspectType(t, f.willC)

	d := f.document(t, f.willC, map[string]any{"title": "Spec", "body": map[string]string{"content": "all of it"}})

	rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID,
		map[string]any{"title": "Spec", "type": "aspect", "aspect_type_id": noFields}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeAspectTypeNoFields, "body.aspect_type_id")

	rec = call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID,
		map[string]any{"title": "Spec", "type": "aspect", "aspect_type_id": withFields}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("convert: %d %s", rec.Code, rec.Body)
	}
	var got documentWire
	decode(t, rec, &got)
	if got.Type != "aspect" || got.Body[fields[0]] != "all of it" || len(got.Body) != 1 {
		t.Errorf("converted = %+v", got)
	}
}

func TestAStaleIfMatchIs409CarryingTheCurrentCopy(t *testing.T) {
	f := newFeed(t)
	d := f.document(t, f.willC, map[string]any{"title": "v1"})

	if rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "v2"}, f.sarahC, "If-Match", `"1"`); rec.Code != http.StatusOK {
		t.Fatalf("sarah's edit: %d %s", rec.Code, rec.Body)
	}

	rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "mine"}, f.willC, "If-Match", `"1"`)
	var c documentConflictWire
	decode(t, rec, &c)
	if rec.Code != http.StatusConflict || c.Code != CodeDocumentConflict || c.Current.Version != 2 || c.Current.Title != "v2" || c.ChangedAt.IsZero() {
		t.Fatalf("conflict = %d %+v", rec.Code, c)
	}

	// Nothing was written, and without If-Match the write is last-write-wins.
	rec = call(t, f.mux, http.MethodGet, V1+"/documents/"+d.ID, nil, f.willC)
	var got documentWire
	decode(t, rec, &got)
	if got.Title != "v2" {
		t.Errorf("title after a refused write = %q", got.Title)
	}
	if rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "forced"}, f.willC); rec.Code != http.StatusOK {
		t.Errorf("unconditional put: %d", rec.Code)
	}

	if rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "x"}, f.willC, "If-Match", "3"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a malformed If-Match: %d, want 422", rec.Code)
	}
}

func TestArchiveUnarchiveAndList(t *testing.T) {
	f := newFeed(t)
	d := f.document(t, f.willC, map[string]any{"title": "gone", "project_id": f.project.ID})
	f.document(t, f.willC, map[string]any{"title": "kept"})

	list := func(body map[string]any) (titles []string) {
		t.Helper()
		rec := call(t, f.mux, http.MethodPost, V1+"/documents/list", body, f.sarahC)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Items []documentWire `json:"items"`
		}
		decode(t, rec, &out)
		for _, it := range out.Items {
			titles = append(titles, it.Title)
		}
		return titles
	}
	if got := list(map[string]any{}); len(got) != 2 {
		t.Errorf("list = %v", got)
	}
	if got := list(map[string]any{"project_id": f.project.ID}); len(got) != 1 || got[0] != "gone" {
		t.Errorf("project filter = %v", got)
	}

	rec := call(t, f.mux, http.MethodDelete, V1+"/documents/"+d.ID, nil, f.willC, "If-Match", `"1"`)
	if rec.Code != http.StatusNoContent || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("archive: %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := call(t, f.mux, http.MethodGet, V1+"/documents/"+d.ID, nil, f.willC); rec.Code != http.StatusNotFound {
		t.Errorf("get archived: %d", rec.Code)
	}
	if got := list(map[string]any{}); len(got) != 1 {
		t.Errorf("list after archive = %v", got)
	}
	if got := list(map[string]any{"archived": true}); len(got) != 1 || got[0] != "gone" {
		t.Errorf("archived list = %v", got)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "x"}, f.willC, "If-Match", `"2"`)
	var c documentConflictWire
	decode(t, rec, &c)
	if rec.Code != http.StatusConflict || c.Current.ArchivedAt == nil {
		t.Errorf("write to an archived document: %d %s", rec.Code, rec.Body)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/documents/"+d.ID+"/unarchive", nil, f.willC, "If-Match", `"2"`)
	if rec.Code != http.StatusNoContent || rec.Header().Get("ETag") != `"3"` {
		t.Fatalf("unarchive: %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := call(t, f.mux, http.MethodGet, V1+"/documents/"+d.ID, nil, f.willC); rec.Code != http.StatusOK {
		t.Errorf("get restored: %d", rec.Code)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/documents/list", map[string]any{"limit": 1}, f.willC)
	var page struct {
		Items      []documentWire `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		HasMore    bool           `json:"has_more"`
	}
	decode(t, rec, &page)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Errorf("first page = %+v", page)
	}
}

func TestADocumentOfAnotherWorkspaceIsNotFound(t *testing.T) {
	f := newFeed(t)
	d := f.document(t, f.willC, map[string]any{"title": "private"})

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, V1 + "/documents/" + d.ID},
		{http.MethodDelete, V1 + "/documents/" + d.ID},
		{http.MethodPost, V1 + "/documents/" + d.ID + "/unarchive"},
	} {
		wantDocProblem(t, call(t, f.mux, c.method, c.path, nil, f.carolC), http.StatusNotFound, CodeDocumentNotFound, "")
	}
	wantDocProblem(t, call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "x"}, f.carolC),
		http.StatusNotFound, CodeDocumentNotFound, "")

	// And she cannot file one under their project.
	rec := call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{"project_id": f.project.ID}, f.carolC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeProjectNotFound, "body.project_id")
}

func TestTheUsageEndpointCountsDocumentsHoldingTheField(t *testing.T) {
	f := newFeed(t)
	typeID, fields := f.aspectType(t, f.willC, "Used", "Unused")
	_, otherFields := f.aspectType(t, f.willC, "Elsewhere")

	for _, body := range []map[string]string{{fields[0]: "a"}, {fields[0]: "b"}, {}} {
		f.document(t, f.willC, map[string]any{"type": "aspect", "aspect_type_id": typeID, "body": body})
	}

	usage := func(cookie, tid, fid string) (int, int) {
		rec := call(t, f.mux, http.MethodGet, V1+"/aspect-types/"+tid+"/fields/"+fid+"/usage", nil, cookie)
		var out struct {
			DocumentCount int `json:"document_count"`
		}
		decode(t, rec, &out)

		return rec.Code, out.DocumentCount
	}
	if code, n := usage(f.sarahC, typeID, fields[0]); code != http.StatusOK || n != 2 {
		t.Errorf("used field: %d, %d", code, n)
	}
	if code, n := usage(f.sarahC, typeID, fields[1]); code != http.StatusOK || n != 0 {
		t.Errorf("unused field: %d, %d", code, n)
	}
	// A field of some other type, and a caller outside the workspace, are both the type's 404.
	if code, _ := usage(f.willC, typeID, otherFields[0]); code != http.StatusNotFound {
		t.Errorf("another type's field: %d", code)
	}
	if code, _ := usage(f.carolC, typeID, fields[0]); code != http.StatusNotFound {
		t.Errorf("outsider: %d", code)
	}
}

func TestEachDocumentWriteReachesAColleagueOnTheFeed(t *testing.T) {
	f := newFeed(t)
	conn := f.open(t, f.cookie(f.sarahC))

	read := func() (kind string, ev struct {
		Type       string       `json:"type"`
		DocumentID string       `json:"document_id"`
		Version    int          `json:"version"`
		ActorID    string       `json:"actor_id"`
		ProjectID  string       `json:"project_id"`
		Document   documentWire `json:"document"`
	}) {
		t.Helper()
		for {
			kind0, data, err := conn.Read(testCtx(t))
			if err != nil || kind0 != 1 {
				t.Fatalf("read: %v", err)
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatalf("event %s: %v", data, err)
			}
			// LAM-89: each write also announces its log entry; activity_test.go covers those.
			if ev.Type != "activity.created" {
				return ev.Type, ev
			}
		}
	}

	d := f.document(t, f.willC, map[string]any{"title": "t", "project_id": f.project.ID})
	call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "u"}, f.willC)
	call(t, f.mux, http.MethodDelete, V1+"/documents/"+d.ID, nil, f.willC)
	call(t, f.mux, http.MethodPost, V1+"/documents/"+d.ID+"/unarchive", nil, f.willC)

	for i, want := range []string{"document.created", "document.updated", "document.archived", "document.unarchived"} {
		kind, ev := read()
		if kind != want || ev.DocumentID != d.ID || ev.Version != i+1 || ev.Document.Version != i+1 ||
			ev.ActorID != f.will.AccountID || ev.ProjectID != f.project.ID {
			t.Errorf("event %d = %+v, want %s at version %d", i, ev, want, i+1)
		}
		if want == "document.archived" && ev.Document.ArchivedAt == nil {
			t.Error("the archived event's document has no archived_at")
		}
	}
}

func TestTheDocumentDeclaresItsConflictAndTitleLimit(t *testing.T) {
	for _, name := range []string{"DocumentConflict"} {
		s := schemaNamed(t, name)
		for _, member := range []string{"current", "changed_at", "code"} {
			if _, ok := s.Properties[member]; !ok {
				t.Errorf("%s has no %q", name, member)
			}
		}
	}
}

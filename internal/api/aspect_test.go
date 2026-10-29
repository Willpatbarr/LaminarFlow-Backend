package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"

	"github.com/danielgtaylor/huma/v2"
)

// Eleven operations (the ninth is the field usage count, LAM-87; LAM-110 added export and import), and one of them being public would be a silent hole. Marked
// individually rather than wrapped, so this checks the marking rather than trusting it.
func TestEveryAspectOperationRequiresACaller(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	found := 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, V1+"/aspect-types") {
			continue
		}

		for method, op := range map[string]*huma.Operation{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put, "DELETE": item.Delete,
		} {
			if op == nil {
				continue
			}
			found++

			if required, _ := op.Metadata[RequireAuth].(bool); !required {
				t.Errorf("%s %s is not marked RequireAuth, so it is public", method, path)
			}
		}
	}

	if found != 11 {
		t.Errorf("found %d aspect operations, want 11", found)
	}
}

// A reorder naming the wrong set is 409, not 422: the request is well formed and would
// have been right a moment ago, so the client refetches and retries rather than
// treating its own body as malformed.
func TestAWrongFieldSetIsAConflict(t *testing.T) {
	var e *Error
	if !errors.As(aspectError(aspect.ErrFieldSetWrong), &e) {
		t.Fatal("ErrFieldSetWrong did not map to the envelope")
	}

	if e.Status != http.StatusConflict || e.Code != CodeFieldSetWrong {
		t.Errorf("status %d code %q, want 409 %q", e.Status, e.Code, CodeFieldSetWrong)
	}
}

// One 404 for a missing type and a missing field alike, matching the collapse the
// service makes.
func TestAMissingAspectTypeOrFieldIsOne404(t *testing.T) {
	var e *Error
	if !errors.As(aspectError(aspect.ErrNotFound), &e) {
		t.Fatal("ErrNotFound did not map to the envelope")
	}

	if e.Status != http.StatusNotFound || e.Code != CodeAspectNotFound {
		t.Errorf("status %d code %q, want 404 %q", e.Status, e.Code, CodeAspectNotFound)
	}
}

func TestAnUnknownAspectErrorIsNotTurnedIntoAClientError(t *testing.T) {
	boom := errors.New("connection refused")

	if got := aspectError(boom); !errors.Is(got, boom) {
		t.Errorf("aspectError swallowed an unrelated error: %v", got)
	}
}

// The delete decision has to reach whoever calls the API, not only whoever reads the
// service.
func TestTheRemoveFieldOperationSaysWhatItDoesNotDelete(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	op := doc.Paths[V1+"/aspect-types/{id}/fields/{field_id}"].Delete
	if op == nil {
		t.Fatal("remove-field is not registered")
	}

	for _, want := range []string{"stay in every document", "unrecoverable"} {
		if !strings.Contains(op.Description, want) {
			t.Errorf("the description does not say %q", want)
		}
	}
}

// LAM-110. Export and import over HTTP: the wire shape, the scopes, the 422 locations.

type portableWire struct {
	FormatVersion int    `json:"format_version"`
	Name          string `json:"name"`
	Fields        []struct {
		Label    string `json:"label"`
		Position int    `json:"position"`
	} `json:"fields"`
}

type importedWire struct {
	ID     string `json:"id"`
	TeamID string `json:"team_id"`
	Name   string `json:"name"`
	Fields []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"fields"`
}

func importBody(team string, extra map[string]any) map[string]any {
	body := map[string]any{
		"team_id": team, "format_version": 1, "name": "Class",
		"fields": []map[string]any{{"label": "Purpose", "position": 1}},
	}
	for k, v := range extra {
		body[k] = v
	}

	return body
}

func errorLocation(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var e struct {
		Errors []struct {
			Location string `json:"location"`
		} `json:"errors"`
	}
	decode(t, rec, &e)
	if len(e.Errors) == 0 {
		t.Fatalf("no errors[] in %s", rec.Body)
	}

	return e.Errors[0].Location
}

func TestAStarterTypeRoundTripsBetweenTeamsInOrder(t *testing.T) {
	mux, _ := registrationMux(t, false)
	reg, cookie := register(t, mux, "will@example.com")

	a := createTeam(t, mux, cookie, reg.WorkspaceID, "Alpha")
	b := createTeam(t, mux, cookie, reg.WorkspaceID, "Beta")

	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	decode(t, call(t, mux, http.MethodGet, V1+"/aspect-types?team_id="+a.ID, nil, cookie), &list)
	var class string
	for _, it := range list.Items {
		if it.Name == "Class" {
			class = it.ID
		}
	}
	if class == "" {
		t.Fatalf("no starter Class in %+v", list.Items)
	}

	for _, label := range []string{"Purpose", "Methods", "Notes"} {
		if rec := call(t, mux, http.MethodPost, V1+"/aspect-types/"+class+"/fields", map[string]any{"label": label}, cookie); rec.Code != http.StatusOK {
			t.Fatalf("add field: %d %s", rec.Code, rec.Body)
		}
	}

	rec := call(t, mux, http.MethodGet, V1+"/aspect-types/"+class+"/export", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"id"`) {
		t.Errorf("the export carries an id: %s", rec.Body)
	}
	var file portableWire
	decode(t, rec, &file)
	if file.FormatVersion != 1 || file.Name != "Class" || len(file.Fields) != 3 {
		t.Fatalf("export = %+v", file)
	}

	// Team B was seeded with its own Class, so this is also the clash.
	rec = call(t, mux, http.MethodPost, V1+"/aspect-types/import", map[string]any{
		"team_id": b.ID, "format_version": file.FormatVersion, "name": file.Name, "fields": file.Fields,
	}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	var got importedWire
	decode(t, rec, &got)

	if got.TeamID != b.ID || got.ID == class || got.Name != "Class (2)" {
		t.Errorf("imported = %+v", got)
	}
	var ls []string
	for _, f := range got.Fields {
		ls = append(ls, f.Label)
	}
	if !slices.Equal(ls, []string{"Purpose", "Methods", "Notes"}) {
		t.Errorf("labels = %v", ls)
	}
}

func TestAnInvalidImportIs422NamingTheField(t *testing.T) {
	mux, _ := registrationMux(t, false)
	reg, cookie := register(t, mux, "will@example.com")
	team := createTeam(t, mux, cookie, reg.WorkspaceID, "Alpha")

	longLabel := strings.Repeat("a", 101)
	cases := []struct {
		name     string
		body     map[string]any
		code     string
		location string
	}{
		{"unknown version", importBody(team.ID, map[string]any{"format_version": 2}), CodeAspectFormatUnsupported, "body.format_version"},
		{"missing version", func() map[string]any { b := importBody(team.ID, nil); delete(b, "format_version"); return b }(), "unprocessable_entity", "body"},
		{"empty name", importBody(team.ID, map[string]any{"name": ""}), "unprocessable_entity", "body.name"},
		{"blank name", importBody(team.ID, map[string]any{"name": "   "}), "unprocessable_entity", "body.name"},
		{"empty label", importBody(team.ID, map[string]any{"fields": []map[string]any{{"label": "ok", "position": 1}, {"label": "", "position": 2}}}), "unprocessable_entity", "body.fields[1].label"},
		{"long label", importBody(team.ID, map[string]any{"fields": []map[string]any{{"label": longLabel, "position": 1}}}), "unprocessable_entity", "body.fields[0].label"},
		{"unknown key", importBody(team.ID, map[string]any{"color": "red"}), "unprocessable_entity", "body.color"},
		{"unknown field key", importBody(team.ID, map[string]any{"fields": []map[string]any{{"label": "a", "position": 1, "id": "x"}}}), "unprocessable_entity", "body.fields[0].id"},
	}

	for _, c := range cases {
		rec := call(t, mux, http.MethodPost, V1+"/aspect-types/import", c.body, cookie)
		assertCode(t, rec, http.StatusUnprocessableEntity, c.code)
		if got := errorLocation(t, rec); !strings.HasPrefix(got, c.location) {
			t.Errorf("%s: location %q, want %q", c.name, got, c.location)
		}
	}
}

// Registration joins everyone to the one workspace (ADR 0004), so "a team the caller
// cannot use" is reached here with a team that does not exist; the service test covers
// a real team the caller is not a member of.
func TestImportIntoAnUnusableTeamIs404LikeCreate(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")
	ghost := "00000000-0000-4000-8000-000000000000"

	create := call(t, mux, http.MethodPost, V1+"/aspect-types", map[string]any{"team_id": ghost, "name": "X"}, cookie)
	imp := call(t, mux, http.MethodPost, V1+"/aspect-types/import", importBody(ghost, nil), cookie)

	assertCode(t, imp, http.StatusNotFound, CodeAspectNotFound)
	if create.Code != imp.Code {
		t.Errorf("create %d, import %d: they should refuse alike", create.Code, imp.Code)
	}
}

func TestExportOfAMissingTypeIs404AspectTypeNotFound(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodGet, V1+"/aspect-types/00000000-0000-4000-8000-000000000000/export", nil, cookie)
	assertCode(t, rec, http.StatusNotFound, CodeAspectNotFound)
}

// Export reads, import writes: a read-only token can take a file out and not put one in.
func TestExportNeedsAspectReadAndImportNeedsAspectWrite(t *testing.T) {
	mux, _ := registrationMux(t, false)
	reg, cookie := register(t, mux, "will@example.com")
	team := createTeam(t, mux, cookie, reg.WorkspaceID, "Alpha")

	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decode(t, call(t, mux, http.MethodGet, V1+"/aspect-types?team_id="+team.ID, nil, cookie), &list)
	typeID := list.Items[0].ID

	read := createToken(t, mux, cookie, "aspect:read")
	write := createToken(t, mux, cookie, "aspect:write")

	if rec := call(t, mux, http.MethodGet, V1+"/aspect-types/"+typeID+"/export", nil, "", bearer(read.Token)...); rec.Code != http.StatusOK {
		t.Errorf("export with aspect:read: %d %s", rec.Code, rec.Body)
	}
	assertCode(t, call(t, mux, http.MethodGet, V1+"/aspect-types/"+typeID+"/export", nil, "", bearer(write.Token)...), http.StatusForbidden, "insufficient_scope")

	assertCode(t, call(t, mux, http.MethodPost, V1+"/aspect-types/import", importBody(team.ID, nil), "", bearer(read.Token)...), http.StatusForbidden, "insufficient_scope")
	if rec := call(t, mux, http.MethodPost, V1+"/aspect-types/import", importBody(team.ID, nil), "", bearer(write.Token)...); rec.Code != http.StatusOK {
		t.Errorf("import with aspect:write: %d %s", rec.Code, rec.Body)
	}
}

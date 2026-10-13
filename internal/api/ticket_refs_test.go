package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-75. The wire half of the limits, the named-id checks and the optional keys. The
// service half is internal/ticket's refs_test.go.

type problemWire struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Errors []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
	} `json:"errors"`
}

// refused asserts the response is a 422 whose only errors[] entry sits at location, and
// returns it for the code to be checked.
func refused(t *testing.T, rec *httptest.ResponseRecorder, location string) problemWire {
	t.Helper()

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 - body %.300s", rec.Code, rec.Body)
	}

	var p problemWire
	decode(t, rec, &p)
	if len(p.Errors) != 1 || p.Errors[0].Location != location {
		t.Fatalf("errors = %+v, want exactly one at %s - body %.300s", p.Errors, location, rec.Body)
	}

	return p
}

func (f feed) post(t *testing.T, cookie string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/tickets", body, cookie)
}

func (f feed) put(t *testing.T, cookie, id string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPut, V1+"/tickets/"+id, body, cookie)
}

// live is the tickets the cookie's owner can list, i.e. every live one in the workspace.
// Through the API because sqlguard keeps this package out of the ticket table, and
// because "what a client would see" is the honest question anyway.
func (f feed) live(t *testing.T, cookie string) []ticketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/list", map[string]any{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d - %.300s", rec.Code, rec.Body)
	}

	var page pageWire[ticketWire]
	decode(t, rec, &page)

	return page.Items
}

func (f feed) minimal(title string) map[string]any {
	return map[string]any{"project_id": f.project.ID, "title": title}
}

func replace(title string, extra map[string]any) map[string]any {
	body := map[string]any{"title": title, "description": "d", "status_id": nil, "assignee_id": nil}
	for k, v := range extra {
		body[k] = v
	}

	return body
}

func TestAnOverLongTitleIsA422AtBodyTitle(t *testing.T) {
	f := newFeed(t)
	long := strings.Repeat("a", ticket.MaxTitleLength+1)
	made := f.create(t, f.willC, "Fix login")

	refused(t, f.post(t, f.willC, f.minimal(long)), "body.title")
	refused(t, f.put(t, f.willC, made.ID, replace(long, nil)), "body.title")

	// The case from the end-to-end run: a million characters, which the 1 MiB body cap
	// used to let through. Stays refused, and nothing is stored.
	huge := strings.Repeat("a", 1_000_000)
	if rec := f.post(t, f.willC, f.minimal(huge)); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("1,000,000-character title: status %d", rec.Code)
	}
	if n := len(f.live(t, f.willC)); n != 1 {
		t.Errorf("%d tickets, want only the one from before", n)
	}
}

func TestAnOverLongDescriptionIsA422AtBodyDescription(t *testing.T) {
	f := newFeed(t)
	long := strings.Repeat("d", ticket.MaxDescriptionLength+1)
	made := f.create(t, f.willC, "Fix login")

	body := f.minimal("t")
	body["description"] = long
	refused(t, f.post(t, f.willC, body), "body.description")
	refused(t, f.put(t, f.willC, made.ID, replace("t", map[string]any{"description": long})), "body.description")

	body["description"] = strings.Repeat("d", ticket.MaxDescriptionLength)
	if rec := f.post(t, f.willC, body); rec.Code != http.StatusOK {
		t.Errorf("a description at the limit: status %d - %.200s", rec.Code, rec.Body)
	}
}

func TestABlankTitleIsA422AtBodyTitle(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	for _, title := range []string{"", "   ", "\t\n ", "  "} {
		refused(t, f.post(t, f.willC, f.minimal(title)), "body.title")
		refused(t, f.put(t, f.willC, made.ID, replace(title, nil)), "body.title")
	}
}

func TestATwoHundredCharacterTitleOfMultiByteCharactersIsAccepted(t *testing.T) {
	f := newFeed(t)
	title := strings.Repeat("あ", ticket.MaxTitleLength) // 600 bytes
	made := f.create(t, f.willC, "Fix login")

	rec := f.post(t, f.willC, f.minimal(title))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d - %.300s", rec.Code, rec.Body)
	}
	var got ticketWire
	decode(t, rec, &got)
	if got.Title != title {
		t.Error("the title came back altered")
	}

	if rec := f.put(t, f.willC, made.ID, replace(title, nil)); rec.Code != http.StatusOK {
		t.Errorf("update: status %d - %.300s", rec.Code, rec.Body)
	}

	// One more character is refused, so the line is characters and not bytes either way.
	refused(t, f.post(t, f.willC, f.minimal(title+"あ")), "body.title")
}

func TestAnOverLongOrBlankTeamNameIsA422AtBodyName(t *testing.T) {
	f := newFeed(t)

	for name, want := range map[string]bool{
		strings.Repeat("a", team.MaxNameLength+1): false,
		strings.Repeat("あ", team.MaxNameLength):   true,
		"":    false,
		"   ": false,
	} {
		rec := call(t, f.mux, http.MethodPost, V1+"/teams",
			map[string]any{"workspace_id": f.will.WorkspaceID, "name": name}, f.willC)
		if want && rec.Code != http.StatusOK {
			t.Errorf("name of %d characters: status %d - %.200s", len([]rune(name)), rec.Code, rec.Body)
		}
		if !want {
			refused(t, rec, "body.name")
		}
	}
}

func TestAnOverLongOrBlankProjectNameIsA422AtBodyName(t *testing.T) {
	f := newFeed(t)

	for name, want := range map[string]bool{
		strings.Repeat("a", project.MaxNameLength+1): false,
		strings.Repeat("あ", project.MaxNameLength):   true,
		"":     false,
		" \t ": false,
	} {
		rec := call(t, f.mux, http.MethodPost, V1+"/projects",
			map[string]any{"team_id": f.project.TeamID, "name": name}, f.willC)
		if want && rec.Code != http.StatusOK {
			t.Errorf("name of %d characters: status %d - %.200s", len([]rune(name)), rec.Code, rec.Body)
		}
		if !want {
			refused(t, rec, "body.name")
		}
	}
}

// ─── named ids ───────────────────────────────────────────────────────────

const noSuchID = "00000000-0000-4000-8000-000000000001"

func TestAnUnknownStatusIsA422AtBodyStatusID(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	body := f.minimal("t")
	body["status_id"] = noSuchID
	if p := refused(t, f.post(t, f.willC, body), "body.status_id"); p.Code != CodeStatusNotFound {
		t.Errorf("create: code %q, want %q", p.Code, CodeStatusNotFound)
	}
	if p := refused(t, f.put(t, f.willC, made.ID, replace("t", map[string]any{"status_id": noSuchID})), "body.status_id"); p.Code != CodeStatusNotFound {
		t.Errorf("update: code %q, want %q", p.Code, CodeStatusNotFound)
	}
}

func TestAStatusFromAnotherTeamIsTheSameError(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	design := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Design")
	foreign := design.Statuses[0].ID

	body := f.minimal("t")
	body["status_id"] = foreign
	if p := refused(t, f.post(t, f.willC, body), "body.status_id"); p.Code != CodeStatusNotFound {
		t.Errorf("create: code %q", p.Code)
	}
	if p := refused(t, f.put(t, f.willC, made.ID, replace("t", map[string]any{"status_id": foreign})), "body.status_id"); p.Code != CodeStatusNotFound {
		t.Errorf("update: code %q", p.Code)
	}

	// The project's own team's status is accepted, so it is the team that was refused.
	var own teamWire
	decode(t, call(t, f.mux, http.MethodGet, V1+"/teams/"+f.project.TeamID, nil, f.willC), &own)
	body["status_id"] = own.Statuses[0].ID
	if rec := f.post(t, f.willC, body); rec.Code != http.StatusOK {
		t.Errorf("the team's own status: status %d - %.200s", rec.Code, rec.Body)
	}
}

func TestAnUnknownAssigneeIsA422AtBodyAssigneeID(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	body := f.minimal("t")
	body["assignee_id"] = noSuchID
	if p := refused(t, f.post(t, f.willC, body), "body.assignee_id"); p.Code != CodeAssigneeNotFound {
		t.Errorf("create: code %q, want %q", p.Code, CodeAssigneeNotFound)
	}
	if p := refused(t, f.put(t, f.willC, made.ID, replace("t", map[string]any{"assignee_id": noSuchID})), "body.assignee_id"); p.Code != CodeAssigneeNotFound {
		t.Errorf("update: code %q, want %q", p.Code, CodeAssigneeNotFound)
	}
}

// carol is a real account in another workspace. The answer is the one for an id that does
// not exist, so it cannot be used to find out which accounts exist.
func TestAnAssigneeOutsideTheWorkspaceIsTheSameErrorAsAnUnknownOne(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	body := f.minimal("t")
	body["assignee_id"] = f.carol.AccountID
	outside := f.post(t, f.willC, body)
	p := refused(t, outside, "body.assignee_id")
	if p.Code != CodeAssigneeNotFound {
		t.Errorf("code %q, want %q", p.Code, CodeAssigneeNotFound)
	}

	body["assignee_id"] = noSuchID
	unknown := f.post(t, f.willC, body)
	if o, u := normalised(outside.Body.String(), f.carol.AccountID), normalised(unknown.Body.String(), noSuchID); o != u {
		t.Errorf("an outside account and an unknown one are distinguishable:\n  %s\n  %s", o, u)
	}

	refused(t, f.put(t, f.willC, made.ID, replace("t", map[string]any{"assignee_id": f.carol.AccountID})), "body.assignee_id")

	// A colleague is fine.
	body["assignee_id"] = f.sarah.AccountID
	if rec := f.post(t, f.willC, body); rec.Code != http.StatusOK {
		t.Errorf("a colleague: status %d - %.200s", rec.Code, rec.Body)
	}
}

// normalised blanks the id a response may echo, so two responses can be compared for
// everything else.
func normalised(body, id string) string {
	return strings.ReplaceAll(body, id, "<id>")
}

func TestNothingIsStoredByARefusedCreate(t *testing.T) {
	f := newFeed(t)

	body := f.minimal("t")
	body["status_id"] = noSuchID
	f.post(t, f.willC, body)

	if n := len(f.live(t, f.willC)); n != 0 {
		t.Errorf("%d tickets stored by a refused create", n)
	}
}

// ─── optional keys ───────────────────────────────────────────────────────

func TestATicketCanBeCreatedWithoutStatusAssigneeOrDescription(t *testing.T) {
	f := newFeed(t)

	rec := f.post(t, f.willC, f.minimal("Just a title"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d - %.300s", rec.Code, rec.Body)
	}

	var got ticketWire
	decode(t, rec, &got)
	if got.StatusID != nil || got.AssigneeID != nil || got.Description != "" {
		t.Errorf("created %+v, want null status, null assignee, empty description", got)
	}

	// And an explicit null is still fine, so a client that sends them is unaffected.
	body := f.minimal("Explicit nulls")
	body["status_id"], body["assignee_id"], body["description"] = nil, nil, ""
	if rec := f.post(t, f.willC, body); rec.Code != http.StatusOK {
		t.Errorf("explicit nulls: status %d - %.300s", rec.Code, rec.Body)
	}
}

// PUT is a full replace and stays one: the keys are required, null is the value.
func TestUpdateStillRequiresEveryWritableKey(t *testing.T) {
	f := newFeed(t)
	made := f.create(t, f.willC, "Fix login")

	rec := f.put(t, f.willC, made.ID, map[string]any{"title": "t"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("PUT with only a title: status %d, want 422 - %.300s", rec.Code, rec.Body)
	}
}

func TestTeamAndProjectCanBeCreatedWithoutBare(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPost, V1+"/teams",
		map[string]any{"workspace_id": f.will.WorkspaceID, "name": "No bare"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("team: status %d - %.300s", rec.Code, rec.Body)
	}
	var tm teamWire
	decode(t, rec, &tm)
	if len(tm.Statuses) == 0 {
		t.Error("a team created without bare was not seeded: omitted must mean false")
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/projects",
		map[string]any{"team_id": tm.ID, "name": "No bare"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("project: status %d - %.300s", rec.Code, rec.Body)
	}
	var pr struct {
		BoardID *string `json:"board_id"`
	}
	decode(t, rec, &pr)
	if pr.BoardID == nil {
		t.Error("a project created without bare got no board: omitted must mean false")
	}

	// And bare: true still skips the seeding.
	rec = call(t, f.mux, http.MethodPost, V1+"/teams",
		map[string]any{"workspace_id": f.will.WorkspaceID, "name": "Bare", "bare": true}, f.willC)
	decode(t, rec, &tm)
	if len(tm.Statuses) != 0 {
		t.Errorf("a bare team has %d statuses", len(tm.Statuses))
	}
}

// ─── the document ────────────────────────────────────────────────────────

func TestTheOpenAPILimitsAreTheConstants(t *testing.T) {
	for _, tc := range []struct {
		schema, field string
		want          int
	}{
		{"CreateTicketInputBody", "title", ticket.MaxTitleLength},
		{"UpdateTicketInputBody", "title", ticket.MaxTitleLength},
		{"CreateTicketInputBody", "description", ticket.MaxDescriptionLength},
		{"UpdateTicketInputBody", "description", ticket.MaxDescriptionLength},
		{"CreateTeamInputBody", "name", team.MaxNameLength},
		{"CreateProjectInputBody", "name", project.MaxNameLength},
	} {
		prop := schemaNamed(t, tc.schema).Properties[tc.field]
		if prop == nil || prop.MaxLength == nil || *prop.MaxLength != tc.want {
			t.Errorf("%s.%s maxLength = %v, want %d", tc.schema, tc.field, prop, tc.want)
		}
	}

	// And the lower bound of the fields that must say something.
	for _, tc := range []struct{ schema, field string }{
		{"CreateTicketInputBody", "title"}, {"UpdateTicketInputBody", "title"},
		{"CreateTeamInputBody", "name"}, {"CreateProjectInputBody", "name"},
	} {
		if prop := schemaNamed(t, tc.schema).Properties[tc.field]; prop.MinLength == nil || *prop.MinLength != 1 {
			t.Errorf("%s.%s has no minLength 1", tc.schema, tc.field)
		}
	}
}

func TestTheOpenAPIDocumentsWhichKeysAreOptional(t *testing.T) {
	create := schemaNamed(t, "CreateTicketInputBody")
	for _, optional := range []string{"status_id", "assignee_id", "description"} {
		if contains(create.Required, optional) {
			t.Errorf("CreateTicketInputBody requires %q", optional)
		}
	}
	// Optional is not the same as not-nullable: a client that sends null keeps working,
	// and its generated type keeps allowing it.
	for _, nullable := range []string{"status_id", "assignee_id"} {
		if p := create.Properties[nullable]; p == nil || !p.Nullable {
			t.Errorf("CreateTicketInputBody.%s is not nullable: %+v", nullable, p)
		}
	}
	for _, required := range []string{"project_id", "title"} {
		if !contains(create.Required, required) {
			t.Errorf("CreateTicketInputBody does not require %q", required)
		}
	}

	update := schemaNamed(t, "UpdateTicketInputBody")
	for _, required := range []string{"title", "description", "status_id", "assignee_id"} {
		if !contains(update.Required, required) {
			t.Errorf("UpdateTicketInputBody (a full replace) does not require %q", required)
		}
	}

	for _, name := range []string{"CreateTeamInputBody", "CreateProjectInputBody"} {
		s := schemaNamed(t, name)
		if contains(s.Required, "bare") {
			t.Errorf("%s still requires bare", name)
		}
		if s.Properties["bare"] == nil {
			t.Errorf("%s lost bare", name)
		}
	}
}

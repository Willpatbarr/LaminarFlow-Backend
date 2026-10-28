package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// LAM-65. The wire half of conflict detection: the header, the 409 body, and that the
// document the frontend generates its types from says what the server sends.

type ticketWire struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	StatusID    *string   `json:"status_id"`
	AssigneeID  *string   `json:"assignee_id"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   *string   `json:"updated_by"`
	Version     int       `json:"version"`
}

type conflictWire struct {
	Title     string     `json:"title"`
	Status    int        `json:"status"`
	Detail    string     `json:"detail"`
	Code      string     `json:"code"`
	Current   ticketWire `json:"current"`
	ChangedBy *struct {
		AccountID   string `json:"account_id"`
		DisplayName string `json:"display_name"`
	} `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
}

// editors is two people in one workspace and a ticket they both have open.
type editors struct {
	mux           *http.ServeMux
	will, sarah   registered
	willC, sarahC string
	ticket        ticketWire
	ticketPath    string
}

func registerNamed(t *testing.T, mux *http.ServeMux, email, name string) (registered, string) {
	t.Helper()

	rec := call(t, mux, http.MethodPost, V1+"/auth/register", map[string]string{
		"email": email, "password": "correct horse battery", "display_name": name,
	}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", email, rec.Code, rec.Body)
	}

	var out registered
	decode(t, rec, &out)

	return out, sessionCookieOf(t, rec)
}

func newEditors(t *testing.T) editors {
	t.Helper()

	mux, _ := registrationMux(t, false)

	e := editors{mux: mux}
	e.will, e.willC = registerNamed(t, mux, "will@example.com", "Will Barr")
	e.sarah, e.sarahC = registerNamed(t, mux, "sarah@example.com", "Sarah Okafor")

	team := createTeam(t, mux, e.willC, e.will.WorkspaceID, "Platform")
	project := createProject(t, mux, e.willC, team.ID, "Core")

	rec := call(t, mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": project.ID, "title": "Fix login", "description": "why it matters",
		"status_id": nil, "assignee_id": nil,
	}, e.willC)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create ticket: status %d, body %s", rec.Code, rec.Body)
	}
	decode(t, rec, &e.ticket)

	e.ticketPath = V1 + "/tickets/" + e.ticket.ID

	return e
}

func (e editors) put(t *testing.T, cookie, title, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()

	// status_id and assignee_id are required members of the body (null is the value),
	// which is a PUT's full-replace made literal.
	body := map[string]any{"title": title, "description": "d", "status_id": nil, "assignee_id": nil}
	if ifMatch == "" {
		return call(t, e.mux, http.MethodPut, e.ticketPath, body, cookie)
	}

	return call(t, e.mux, http.MethodPut, e.ticketPath, body, cookie, "If-Match", ifMatch)
}

func (e editors) send(t *testing.T, method, path, cookie, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()

	if ifMatch == "" {
		return call(t, e.mux, method, path, nil, cookie)
	}

	return call(t, e.mux, method, path, nil, cookie, "If-Match", ifMatch)
}

func TestCreateReportsVersionOneAndWhoWroteIt(t *testing.T) {
	e := newEditors(t)

	if e.ticket.Version != 1 {
		t.Errorf("version = %d, want 1", e.ticket.Version)
	}
	if e.ticket.UpdatedBy == nil || *e.ticket.UpdatedBy != e.will.AccountID {
		t.Errorf("updated_by = %v, want the creator", e.ticket.UpdatedBy)
	}
	if e.ticket.UpdatedAt.IsZero() {
		t.Error("updated_at is missing")
	}

	// The same version is the ETag, quoted as If-Match takes it back.
	rec := call(t, e.mux, http.MethodGet, e.ticketPath, nil, e.willC)
	if got := rec.Header().Get("ETag"); got != `"1"` {
		t.Errorf("ETag = %q, want %q", got, `"1"`)
	}
}

func TestAnUpdateAtTheCurrentVersionSucceedsAndReturnsTheNext(t *testing.T) {
	e := newEditors(t)

	rec := e.put(t, e.willC, "Fix login properly", `"1"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}

	var got ticketWire
	decode(t, rec, &got)

	if got.Version != 2 || rec.Header().Get("ETag") != `"2"` {
		t.Errorf("version %d, ETag %q, want 2 and \"2\"", got.Version, rec.Header().Get("ETag"))
	}
}

// The ticket's wording, end to end: "edited by Sarah 2 minutes ago" needs a name and a
// time, and the server's copy to show beside the local one.
func TestAStaleUpdateIsA409NamingWhoEditedAndCarryingTheirCopy(t *testing.T) {
	e := newEditors(t)

	if rec := e.put(t, e.sarahC, "Sarah's title", `"1"`); rec.Code != http.StatusOK {
		t.Fatalf("sarah's update: status %d, body %s", rec.Code, rec.Body)
	}

	rec := e.put(t, e.willC, "Will's title", `"1"`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("content type = %q, want problem+json", ct)
	}

	var got conflictWire
	decode(t, rec, &got)

	if got.Code != CodeTicketConflict || got.Status != http.StatusConflict || got.Title != "Conflict" {
		t.Errorf("envelope = %q %d %q, want ticket_conflict 409 Conflict", got.Code, got.Status, got.Title)
	}
	if got.Current.Title != "Sarah's title" || got.Current.Version != 2 || got.Current.ID != e.ticket.ID {
		t.Errorf("current = %+v, want Sarah's ticket at version 2", got.Current)
	}
	if got.Current.UpdatedBy == nil || *got.Current.UpdatedBy != e.sarah.AccountID {
		t.Errorf("current.updated_by = %v, want Sarah", got.Current.UpdatedBy)
	}
	if got.ChangedBy == nil || got.ChangedBy.AccountID != e.sarah.AccountID || got.ChangedBy.DisplayName != "Sarah Okafor" {
		t.Errorf("changed_by = %+v, want Sarah Okafor", got.ChangedBy)
	}
	if !got.ChangedAt.Equal(got.Current.UpdatedAt) || time.Since(got.ChangedAt) > time.Minute {
		t.Errorf("changed_at = %v, want Sarah's edit time %v", got.ChangedAt, got.Current.UpdatedAt)
	}

	// And Will's edit changed nothing.
	var read ticketWire
	decode(t, call(t, e.mux, http.MethodGet, e.ticketPath, nil, e.willC), &read)
	if read.Title != "Sarah's title" || read.Version != 2 {
		t.Errorf("ticket = %q at %d after the refused write, want Sarah's at 2", read.Title, read.Version)
	}
}

func TestUpdatingAnArchivedTicketIsA409ArchivedNamingTheArchiverAndStillCarryingIt(t *testing.T) {
	e := newEditors(t)

	arch := e.send(t, http.MethodDelete, e.ticketPath, e.sarahC, `"1"`)
	if arch.Code != http.StatusNoContent {
		t.Fatalf("archive: status %d, body %s", arch.Code, arch.Body)
	}
	// The 204 has no body, so the new version has to arrive in the header.
	if got := arch.Header().Get("ETag"); got != `"2"` {
		t.Errorf("archive ETag = %q, want \"2\"", got)
	}

	rec := e.put(t, e.willC, "edited on the train", `"1"`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}

	var got conflictWire
	decode(t, rec, &got)

	if got.Code != CodeTicketArchived {
		t.Errorf("code = %q, want ticket_archived", got.Code)
	}
	if got.Current.ID != e.ticket.ID || got.Current.Title != "Fix login" || got.Current.Version != 2 {
		t.Errorf("current = %+v, want the archived ticket at version 2", got.Current)
	}
	if got.ChangedBy == nil || got.ChangedBy.DisplayName != "Sarah Okafor" {
		t.Errorf("changed_by = %+v, want the archiver", got.ChangedBy)
	}

	// Reads are unchanged.
	if rec := call(t, e.mux, http.MethodGet, e.ticketPath, nil, e.willC); rec.Code != http.StatusNotFound {
		t.Errorf("GET of an archived ticket = %d, want 404 as before", rec.Code)
	}
}

// What an API-token script sees is exactly what it saw before.
func TestWithoutIfMatchNothingChanges(t *testing.T) {
	e := newEditors(t)

	if rec := e.put(t, e.sarahC, "Sarah's title", ""); rec.Code != http.StatusOK {
		t.Fatalf("sarah: %d %s", rec.Code, rec.Body)
	}
	rec := e.put(t, e.willC, "Will wins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a stale write with no precondition = %d, want 200 (last write wins); %s", rec.Code, rec.Body)
	}

	if rec := e.send(t, http.MethodDelete, e.ticketPath, e.sarahC, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("archive with no precondition = %d, want 204; %s", rec.Code, rec.Body)
	}

	rec = e.put(t, e.willC, "too late", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update of an archived ticket with no precondition = %d, want 404; %s", rec.Code, rec.Body)
	}
	var env struct {
		Code string `json:"code"`
	}
	decode(t, rec, &env)
	if env.Code != CodeTicketNotFound {
		t.Errorf("code = %q, want ticket_not_found", env.Code)
	}

	// Archiving twice and restoring a live ticket are the not-founds they have always been.
	if rec := e.send(t, http.MethodDelete, e.ticketPath, e.willC, ""); rec.Code != http.StatusNotFound {
		t.Errorf("archive twice = %d, want 404", rec.Code)
	}
}

func TestArchiveAndUnarchiveTakeTheSamePrecondition(t *testing.T) {
	e := newEditors(t)
	restore := e.ticketPath + "/unarchive"

	// Sarah edits; Will archives from the version he last saw.
	e.put(t, e.sarahC, "Sarah's title", `"1"`)

	rec := e.send(t, http.MethodDelete, e.ticketPath, e.willC, `"1"`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale archive = %d, want 409; %s", rec.Code, rec.Body)
	}
	var got conflictWire
	decode(t, rec, &got)
	if got.Code != CodeTicketConflict || got.Current.Title != "Sarah's title" {
		t.Errorf("stale archive was told %q / %q", got.Code, got.Current.Title)
	}

	// At the right version it goes through, and the ETag is the next one.
	rec = e.send(t, http.MethodDelete, e.ticketPath, e.willC, `"2"`)
	if rec.Code != http.StatusNoContent || rec.Header().Get("ETag") != `"3"` {
		t.Fatalf("archive at 2 = %d, ETag %q, want 204 and \"3\"", rec.Code, rec.Header().Get("ETag"))
	}

	// A retry of Will's own archive, whose response was lost: archived, by Will himself.
	rec = e.send(t, http.MethodDelete, e.ticketPath, e.willC, `"2"`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retried archive = %d, want 409", rec.Code)
	}
	decode(t, rec, &got)
	if got.Code != CodeTicketArchived || got.ChangedBy == nil || got.ChangedBy.AccountID != e.will.AccountID {
		t.Errorf("retried archive was told %q by %+v, want ticket_archived by Will", got.Code, got.ChangedBy)
	}

	// Restore from a stale view of the archive...
	if rec := e.send(t, http.MethodPost, restore, e.sarahC, `"2"`); rec.Code != http.StatusConflict {
		t.Errorf("unarchive at a version that is not the archive's = %d, want 409", rec.Code)
	}
	// ...and from the right one.
	rec = e.send(t, http.MethodPost, restore, e.sarahC, `"3"`)
	if rec.Code != http.StatusNoContent || rec.Header().Get("ETag") != `"4"` {
		t.Fatalf("unarchive at 3 = %d, ETag %q, want 204 and \"4\"", rec.Code, rec.Header().Get("ETag"))
	}

	// Restoring what is not archived stays a not-found, with or without the header.
	for _, header := range []string{"", `"4"`} {
		if rec := e.send(t, http.MethodPost, restore, e.sarahC, header); rec.Code != http.StatusNotFound {
			t.Errorf("unarchive of a live ticket (If-Match %q) = %d, want 404", header, rec.Code)
		}
	}
}

// A malformed precondition is refused, never guessed at: a guessed precondition is how a
// write that should have been refused goes through.
func TestAMalformedIfMatchIsRefused(t *testing.T) {
	e := newEditors(t)

	for _, bad := range []string{`1`, `W/"1"`, `*`, `"0"`, `"abc"`, `"1", "2"`, `"01"`, `"99999999999"`, `""`} {
		rec := e.put(t, e.willC, "x", bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("If-Match %s = %d, want 422", bad, rec.Code)
		}
	}

	// And nothing was written by any of them.
	var read ticketWire
	decode(t, call(t, e.mux, http.MethodGet, e.ticketPath, nil, e.willC), &read)
	if read.Version != 1 {
		t.Errorf("version = %d, a refused precondition wrote something", read.Version)
	}
}

// The list reuses TicketBody, and the client hydrates its versions from it.
func TestTheListCarriesVersionAndWhoLastWrote(t *testing.T) {
	e := newEditors(t)

	e.put(t, e.sarahC, "Sarah's title", `"1"`)

	rec := call(t, e.mux, http.MethodPost, V1+"/tickets/list", map[string]any{
		"filter": map[string]any{"op": "and", "conditions": []map[string]any{
			{"field": "version", "op": "gte", "value": 2},
		}},
	}, e.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	var page pageWire[ticketWire]
	decode(t, rec, &page)

	if len(page.Items) != 1 {
		t.Fatalf("version >= 2 returned %d items, want 1", len(page.Items))
	}
	if got := page.Items[0]; got.Version != 2 || got.UpdatedBy == nil || *got.UpdatedBy != e.sarah.AccountID {
		t.Errorf("listed %+v, want version 2 by Sarah", got)
	}
}

// The document is what the frontend's types are generated from. A member that is on the
// wire and not here does not exist as far as the client is concerned.
func TestTheDocumentDeclaresTheConflictBodyOnEveryWriteThatCanRaiseIt(t *testing.T) {
	ops := operations(t)

	for _, key := range []string{
		"PUT " + V1 + "/tickets/{id}",
		"DELETE " + V1 + "/tickets/{id}",
		"POST " + V1 + "/tickets/{id}/unarchive",
	} {
		op := ops[key]
		if op == nil {
			t.Fatalf("%s is not registered", key)
		}

		resp := op.Responses["409"]
		if resp == nil || resp.Content["application/problem+json"] == nil ||
			resp.Content["application/problem+json"].Schema.Ref != "#/components/schemas/TicketConflict" {
			t.Errorf("%s does not document its 409 as TicketConflict", key)
		}
		if op.Responses["default"] == nil {
			t.Errorf("%s lost its default error response by declaring a 409", key)
		}

		var header *huma.Param
		for _, p := range op.Parameters {
			if p.In == "header" && p.Name == "If-Match" {
				header = p
			}
		}
		if header == nil {
			t.Errorf("%s does not document the If-Match header", key)
		} else if header.Required {
			t.Errorf("%s makes If-Match required; omitting it must stay legal", key)
		}
	}
}

func TestTheConflictSchemaCarriesTheExtensionMembers(t *testing.T) {
	schema := schemaNamed(t, "TicketConflict")

	for _, member := range []string{"code", "current", "changed_by", "changed_at", "detail", "status", "title"} {
		if _, ok := schema.Properties[member]; !ok {
			t.Errorf("TicketConflict has no %q property", member)
		}
	}
	for _, required := range []string{"code", "current", "changed_by", "changed_at"} {
		if !contains(schema.Required, required) {
			t.Errorf("TicketConflict does not require %q", required)
		}
	}

	if ref := schema.Properties["current"].Ref; ref != "#/components/schemas/TicketBody" {
		t.Errorf("current is %q, want TicketBody: same shape as GET", ref)
	}

	// changed_by is null for a ticket whose writer was never recorded.
	by := schema.Properties["changed_by"]
	if by == nil || !by.Nullable {
		t.Errorf("changed_by = %+v, want a nullable object", by)
	}

	ticket := schemaNamed(t, "TicketBody")
	for _, member := range []string{"updated_at", "updated_by", "version"} {
		if _, ok := ticket.Properties[member]; !ok {
			t.Errorf("TicketBody has no %q property", member)
		}
	}
}

func schemaNamed(t *testing.T, name string) *huma.Schema {
	t.Helper()

	s := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI().Components.Schemas.Map()[name]
	if s == nil {
		t.Fatalf("schema %s is not in the document", name)
	}

	return s
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

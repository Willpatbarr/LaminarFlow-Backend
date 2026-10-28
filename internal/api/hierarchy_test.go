package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// LAM-93. The hierarchy and label API end to end: epics, sprints and their membership,
// milestones and labels, and the three members a ticket write gained.

type hTicket struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	Title       string   `json:"title"`
	EpicID      *string  `json:"epic_id"`
	MilestoneID *string  `json:"milestone_id"`
	LabelIDs    []string `json:"label_ids"`
	Version     int      `json:"version"`
}

type hParent struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"project_id"`
	TeamID     string  `json:"team_id"`
	Name       string  `json:"name"`
	Color      string  `json:"color"`
	StartDate  *string `json:"start_date"`
	EndDate    *string `json:"end_date"`
	TargetDate *string `json:"target_date"`
}

// make creates a resource and returns it, failing the test on anything but a 200.
func (f feed) make(t *testing.T, path string, body map[string]any) hParent {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+path, body, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status %d - %.300s", path, rec.Code, rec.Body)
	}
	var out hParent
	decode(t, rec, &out)

	return out
}

func (f feed) epic(t *testing.T, name string) hParent {
	return f.make(t, "/epics", map[string]any{"project_id": f.project.ID, "name": name})
}

func (f feed) milestone(t *testing.T, name string) hParent {
	return f.make(t, "/milestones", map[string]any{"project_id": f.project.ID, "name": name})
}

func (f feed) sprint(t *testing.T, name string) hParent {
	return f.make(t, "/sprints", map[string]any{"project_id": f.project.ID, "name": name})
}

func (f feed) label(t *testing.T, name string) hParent {
	return f.make(t, "/labels", map[string]any{"team_id": f.project.TeamID, "name": name, "color": "#ff0000"})
}

// tk creates a ticket in the feed's project with the given extra members.
func (f feed) tk(t *testing.T, title string, extra map[string]any) hTicket {
	t.Helper()

	body := f.minimal(title)
	for k, v := range extra {
		body[k] = v
	}
	rec := f.post(t, f.willC, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create %q: status %d - %.300s", title, rec.Code, rec.Body)
	}
	var out hTicket
	decode(t, rec, &out)

	return out
}

func (f feed) hGet(t *testing.T, id string) hTicket {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/tickets/"+id, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get ticket: status %d - %.300s", rec.Code, rec.Body)
	}
	var out hTicket
	decode(t, rec, &out)

	return out
}

// hPut is a PUT of title plus the required members, plus extra.
func (f feed) hPut(t *testing.T, id string, extra map[string]any, header ...string) (int, hTicket, []byte) {
	t.Helper()

	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+id, replace("t", extra), f.willC, header...)
	var out hTicket
	if rec.Code == http.StatusOK {
		decode(t, rec, &out)
	}

	return rec.Code, out, rec.Body.Bytes()
}

// inFilter lists the ticket ids matching one condition, in the order the list returns them.
func (f feed) matching(t *testing.T, conds ...map[string]any) []string {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets/list",
		map[string]any{"filter": map[string]any{"op": "and", "conditions": conds}}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d - %.300s", rec.Code, rec.Body)
	}
	var page pageWire[hTicket]
	decode(t, rec, &page)

	var ids []string
	for _, tk := range page.Items {
		ids = append(ids, tk.ID)
	}
	slices.Sort(ids)

	return ids
}

func eq(field, value string) map[string]any {
	return map[string]any{"field": field, "op": "eq", "value": value}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)

	return ids
}

func TestEveryHierarchyOperationRequiresACaller(t *testing.T) {
	f := newFeed(t)
	id := "00000000-0000-0000-0000-000000000001"

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/epics"}, {http.MethodPost, "/epics/list"},
		{http.MethodGet, "/epics/" + id}, {http.MethodPut, "/epics/" + id}, {http.MethodDelete, "/epics/" + id},
		{http.MethodPost, "/sprints"}, {http.MethodPost, "/sprints/list"},
		{http.MethodGet, "/sprints/" + id}, {http.MethodPut, "/sprints/" + id}, {http.MethodDelete, "/sprints/" + id},
		{http.MethodPost, "/sprints/" + id + "/tickets"}, {http.MethodDelete, "/sprints/" + id + "/tickets/" + id},
		{http.MethodPost, "/milestones"}, {http.MethodPost, "/milestones/list"},
		{http.MethodGet, "/milestones/" + id}, {http.MethodPut, "/milestones/" + id}, {http.MethodDelete, "/milestones/" + id},
		{http.MethodPost, "/labels"}, {http.MethodPost, "/labels/list"},
		{http.MethodGet, "/labels/" + id}, {http.MethodPut, "/labels/" + id}, {http.MethodDelete, "/labels/" + id},
	} {
		if rec := call(t, f.mux, c.method, V1+c.path, map[string]any{}, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a caller: status %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

func TestEachParentHasCreateGetListUpdateAndDelete(t *testing.T) {
	f := newFeed(t)

	for _, c := range []struct {
		base, scopeKey, scopeID string
		create                  map[string]any
		update                  map[string]any
		notFound                string
	}{
		{"/epics", "project_id", f.project.ID, map[string]any{"name": "Auth"}, map[string]any{"name": "Auth 2", "description": "d"}, "epic_not_found"},
		{"/milestones", "project_id", f.project.ID, map[string]any{"name": "1.0", "target_date": "2027-01-31"}, map[string]any{"name": "1.1", "target_date": nil}, "milestone_not_found"},
		{"/sprints", "project_id", f.project.ID, map[string]any{"name": "S1", "start_date": "2027-01-04", "end_date": "2027-01-17"}, map[string]any{"name": "S1b", "end_date": nil}, "sprint_not_found"},
		{"/labels", "team_id", f.project.TeamID, map[string]any{"name": "bug", "color": "red"}, map[string]any{"name": "defect", "color": "orange"}, "label_not_found"},
	} {
		t.Run(c.base, func(t *testing.T) {
			body := map[string]any{c.scopeKey: c.scopeID}
			for k, v := range c.create {
				body[k] = v
			}
			rec := call(t, f.mux, http.MethodPost, V1+c.base, body, f.willC)
			if rec.Code != http.StatusOK {
				t.Fatalf("create: status %d - %.300s", rec.Code, rec.Body)
			}
			var made hParent
			decode(t, rec, &made)

			rec = call(t, f.mux, http.MethodGet, V1+c.base+"/"+made.ID, nil, f.willC)
			var got hParent
			decode(t, rec, &got)
			if rec.Code != http.StatusOK || !reflect.DeepEqual(got, made) {
				t.Fatalf("get: status %d, %+v, want %+v", rec.Code, got, made)
			}

			rec = call(t, f.mux, http.MethodPost, V1+c.base+"/list", map[string]any{c.scopeKey: c.scopeID}, f.willC)
			var page pageWire[hParent]
			decode(t, rec, &page)
			if rec.Code != http.StatusOK || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], made) || page.HasMore || page.NextCursor != nil {
				t.Fatalf("list: status %d, %+v, want just %+v on one page", rec.Code, page, made)
			}

			rec = call(t, f.mux, http.MethodPut, V1+c.base+"/"+made.ID, c.update, f.willC)
			if rec.Code != http.StatusOK {
				t.Fatalf("update: status %d - %.300s", rec.Code, rec.Body)
			}
			var upd hParent
			decode(t, rec, &upd)
			if upd.Name != c.update["name"] {
				t.Errorf("updated name %q, want %q", upd.Name, c.update["name"])
			}

			// A blank name is refused wherever the name is a member.
			rec = call(t, f.mux, http.MethodPut, V1+c.base+"/"+made.ID, map[string]any{"name": "   "}, f.willC)
			refused(t, rec, "body.name")

			rec = call(t, f.mux, http.MethodDelete, V1+c.base+"/"+made.ID, nil, f.willC)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("delete: status %d - %.300s", rec.Code, rec.Body)
			}

			rec = call(t, f.mux, http.MethodGet, V1+c.base+"/"+made.ID, nil, f.willC)
			wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, c.notFound, "")
			rec = call(t, f.mux, http.MethodDelete, V1+c.base+"/"+made.ID, nil, f.willC)
			wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, c.notFound, "")
		})
	}
}

func TestMilestoneAndSprintDatesCanBeCleared(t *testing.T) {
	f := newFeed(t)

	m := f.make(t, "/milestones", map[string]any{"project_id": f.project.ID, "name": "1.0", "target_date": "2027-03-01"})
	if m.TargetDate == nil || *m.TargetDate != "2027-03-01" {
		t.Fatalf("target_date %v, want 2027-03-01", m.TargetDate)
	}

	// Omitted leaves it; null clears it.
	rec := call(t, f.mux, http.MethodPut, V1+"/milestones/"+m.ID, map[string]any{"name": "1.0.1"}, f.willC)
	var got hParent
	decode(t, rec, &got)
	if got.TargetDate == nil || *got.TargetDate != "2027-03-01" {
		t.Errorf("after an update that omits it, target_date %v, want it kept", got.TargetDate)
	}
	rec = call(t, f.mux, http.MethodPut, V1+"/milestones/"+m.ID, map[string]any{"target_date": nil}, f.willC)
	decode(t, rec, &got)
	if got.TargetDate != nil || got.Name != "1.0.1" {
		t.Errorf("after null, %+v, want no date and the name kept", got)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/milestones", map[string]any{"project_id": f.project.ID, "name": "x", "target_date": "soon"}, f.willC)
	refused(t, rec, "body.target_date")
}

func TestASprintEndingBeforeItStartsIsA422AtEndDate(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPost, V1+"/sprints",
		map[string]any{"project_id": f.project.ID, "name": "S", "start_date": "2027-02-10", "end_date": "2027-02-09"}, f.willC)
	refused(t, rec, "body.end_date")

	// One date alone is fine, and so is the same day.
	f.make(t, "/sprints", map[string]any{"project_id": f.project.ID, "name": "open", "start_date": "2027-02-10"})
	s := f.make(t, "/sprints", map[string]any{"project_id": f.project.ID, "name": "one day", "start_date": "2027-02-10", "end_date": "2027-02-10"})

	// An update is judged against what is stored: moving only the start past the end.
	rec = call(t, f.mux, http.MethodPut, V1+"/sprints/"+s.ID, map[string]any{"start_date": "2027-02-11"}, f.willC)
	refused(t, rec, "body.end_date")

	rec = call(t, f.mux, http.MethodGet, V1+"/sprints/"+s.ID, nil, f.willC)
	var got hParent
	decode(t, rec, &got)
	if *got.StartDate != "2027-02-10" {
		t.Errorf("start_date %s after a refused update, want it unchanged", *got.StartDate)
	}

	// Clearing the end makes the start free to move.
	rec = call(t, f.mux, http.MethodPut, V1+"/sprints/"+s.ID, map[string]any{"start_date": "2027-02-11", "end_date": nil}, f.willC)
	if rec.Code != http.StatusOK {
		t.Errorf("start moved with the end cleared: status %d - %.300s", rec.Code, rec.Body)
	}
}

func TestLabelNamesAreUniqueWithinATeam(t *testing.T) {
	f := newFeed(t)
	f.label(t, "bug")

	rec := call(t, f.mux, http.MethodPost, V1+"/labels", map[string]any{"team_id": f.project.TeamID, "name": "bug", "color": "x"}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusConflict, "label_name_taken", "")

	feature := f.label(t, "feature")
	rec = call(t, f.mux, http.MethodPut, V1+"/labels/"+feature.ID, map[string]any{"name": "bug"}, f.willC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusConflict, "label_name_taken", "")

	// Renaming to its own name is not a collision, and another team may reuse a name.
	rec = call(t, f.mux, http.MethodPut, V1+"/labels/"+feature.ID, map[string]any{"name": "feature", "color": "blue"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Errorf("rename to itself: status %d - %.300s", rec.Code, rec.Body)
	}
	other := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Other")
	rec = call(t, f.mux, http.MethodPost, V1+"/labels", map[string]any{"team_id": other.ID, "name": "bug", "color": "x"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Errorf("the same name in another team: status %d - %.300s", rec.Code, rec.Body)
	}

	// color is required and bounded.
	rec = call(t, f.mux, http.MethodPost, V1+"/labels", map[string]any{"team_id": f.project.TeamID, "name": "n"}, f.willC)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a label with no color: status %d, want 422", rec.Code)
	}
}

func TestTicketsWithAndWithoutEachParent(t *testing.T) {
	f := newFeed(t)
	epic, other := f.epic(t, "Auth"), f.epic(t, "Billing")
	mile := f.milestone(t, "1.0")
	bug, ux := f.label(t, "bug"), f.label(t, "ux")

	bare := f.tk(t, "bare", nil)
	if bare.EpicID != nil || bare.MilestoneID != nil || bare.LabelIDs == nil || len(bare.LabelIDs) != 0 {
		t.Fatalf("a ticket with no parents = %+v, want null, null and []", bare)
	}

	inEpic := f.tk(t, "in an epic", map[string]any{"epic_id": epic.ID})
	inMile := f.tk(t, "in a milestone", map[string]any{"milestone_id": mile.ID})
	labelled := f.tk(t, "labelled", map[string]any{"label_ids": []string{ux.ID, bug.ID, ux.ID}})
	all := f.tk(t, "all of them", map[string]any{"epic_id": epic.ID, "milestone_id": mile.ID, "label_ids": []string{bug.ID}})

	if inEpic.EpicID == nil || *inEpic.EpicID != epic.ID || inEpic.MilestoneID != nil {
		t.Errorf("in an epic = %+v", inEpic)
	}
	if inMile.MilestoneID == nil || *inMile.MilestoneID != mile.ID || inMile.EpicID != nil {
		t.Errorf("in a milestone = %+v", inMile)
	}
	if want := sorted(bug.ID, ux.ID); !slices.Equal(labelled.LabelIDs, want) {
		t.Errorf("labels %v, want %v (id order, the duplicate once)", labelled.LabelIDs, want)
	}
	if all.EpicID == nil || all.MilestoneID == nil || !slices.Equal(all.LabelIDs, []string{bug.ID}) {
		t.Errorf("all = %+v", all)
	}

	// A read shows what the write returned.
	if got := f.hGet(t, all.ID); got.EpicID == nil || *got.EpicID != epic.ID || !slices.Equal(got.LabelIDs, []string{bug.ID}) {
		t.Errorf("get = %+v", got)
	}

	// Filters: epic and milestone are columns, label is a set.
	if got, want := f.matching(t, eq("epic", epic.ID)), sorted(inEpic.ID, all.ID); !slices.Equal(got, want) {
		t.Errorf("epic filter = %v, want %v", got, want)
	}
	if got, want := f.matching(t, eq("milestone", mile.ID)), sorted(inMile.ID, all.ID); !slices.Equal(got, want) {
		t.Errorf("milestone filter = %v, want %v", got, want)
	}
	if got, want := f.matching(t, eq("label", bug.ID)), sorted(labelled.ID, all.ID); !slices.Equal(got, want) {
		t.Errorf("label filter = %v, want %v", got, want)
	}
	if got := f.matching(t, eq("epic", other.ID)); len(got) != 0 {
		t.Errorf("an epic with no tickets matched %v", got)
	}

	// Update: omitting the three leaves them; null clears; [] clears the labels; an id
	// moves it. Every PUT is a write, so the version rises each time.
	code, kept, body := f.hPut(t, all.ID, nil)
	if code != http.StatusOK || kept.EpicID == nil || kept.MilestoneID == nil || len(kept.LabelIDs) != 1 || kept.Version != all.Version+1 {
		t.Fatalf("a PUT that omits them: status %d, %+v - %.300s", code, kept, body)
	}

	code, moved, body := f.hPut(t, all.ID, map[string]any{"epic_id": other.ID, "milestone_id": nil, "label_ids": []string{ux.ID, bug.ID}})
	if code != http.StatusOK || *moved.EpicID != other.ID || moved.MilestoneID != nil || len(moved.LabelIDs) != 2 {
		t.Fatalf("a PUT that moves them: status %d, %+v - %.300s", code, moved, body)
	}
	if got := f.hGet(t, all.ID); *got.EpicID != other.ID || got.MilestoneID != nil || len(got.LabelIDs) != 2 {
		t.Errorf("after the move, get = %+v", got)
	}

	code, cleared, body := f.hPut(t, all.ID, map[string]any{"epic_id": nil, "label_ids": []string{}})
	if code != http.StatusOK || cleared.EpicID != nil || len(cleared.LabelIDs) != 0 || cleared.LabelIDs == nil {
		t.Fatalf("a PUT that clears them: status %d, %+v - %.300s", code, cleared, body)
	}

	// If-Match still guards a write that carries the new members.
	code, _, body = f.hPut(t, all.ID, map[string]any{"epic_id": epic.ID}, "If-Match", `"1"`)
	wantProblem(t, code, body, http.StatusConflict, "ticket_conflict", "")
	if got := f.hGet(t, all.ID); got.EpicID != nil {
		t.Errorf("a refused conditional write changed the epic to %v", got.EpicID)
	}
}

func TestADanglingParentIsA422AtItsField(t *testing.T) {
	f := newFeed(t)
	epic, mile, bug := f.epic(t, "Auth"), f.milestone(t, "1.0"), f.label(t, "bug")
	made := f.tk(t, "t", map[string]any{"epic_id": epic.ID, "milestone_id": mile.ID, "label_ids": []string{bug.ID}})

	// Another project of the same team, and another team, each with their own parents.
	team2 := createTeam(t, f.mux, f.willC, f.will.WorkspaceID, "Elsewhere")
	proj2 := createProject(t, f.mux, f.willC, f.project.TeamID, "Second")
	foreignEpic := f.make(t, "/epics", map[string]any{"project_id": proj2.ID, "name": "theirs"})
	foreignMile := f.make(t, "/milestones", map[string]any{"project_id": proj2.ID, "name": "theirs"})
	foreignLabel := f.make(t, "/labels", map[string]any{"team_id": team2.ID, "name": "theirs", "color": "x"})
	missing := "00000000-0000-0000-0000-0000000000aa"

	for _, c := range []struct {
		name, code, location string
		extra                map[string]any
	}{
		{"unknown epic", "epic_not_found", "body.epic_id", map[string]any{"epic_id": missing}},
		{"epic of another project", "epic_not_found", "body.epic_id", map[string]any{"epic_id": foreignEpic.ID}},
		{"unknown milestone", "milestone_not_found", "body.milestone_id", map[string]any{"milestone_id": missing}},
		{"milestone of another project", "milestone_not_found", "body.milestone_id", map[string]any{"milestone_id": foreignMile.ID}},
		{"unknown label", "label_not_found", "body.label_ids[1]", map[string]any{"label_ids": []string{bug.ID, missing}}},
		{"label of another team", "label_not_found", "body.label_ids[0]", map[string]any{"label_ids": []string{foreignLabel.ID, bug.ID}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := f.post(t, f.willC, func() map[string]any {
				b := f.minimal("new")
				for k, v := range c.extra {
					b[k] = v
				}
				return b
			}())
			if p := refused(t, rec, c.location); p.Code != c.code {
				t.Errorf("create: code %q, want %q", p.Code, c.code)
			}

			code, _, body := f.hPut(t, made.ID, c.extra)
			wantProblem(t, code, body, http.StatusUnprocessableEntity, c.code, c.location)
		})
	}

	// Nothing from the refused writes landed: no stray ticket, the original untouched.
	if n := len(f.live(t, f.willC)); n != 1 {
		t.Errorf("%d tickets, want only the original", n)
	}
	if got := f.hGet(t, made.ID); got.Version != made.Version || *got.EpicID != epic.ID || len(got.LabelIDs) != 1 {
		t.Errorf("the original = %+v, want it as created (version %d)", got, made.Version)
	}

	// A label id that is not a uuid is the same refusal, not a 500.
	code, _, body := f.hPut(t, made.ID, map[string]any{"label_ids": []string{"not-a-uuid"}})
	wantProblem(t, code, body, http.StatusUnprocessableEntity, "label_not_found", "body.label_ids[0]")

	// Precedence: a stale version is a 409 even when the epic is also wrong.
	code, _, body = f.hPut(t, made.ID, map[string]any{"epic_id": missing}, "If-Match", `"99"`)
	wantProblem(t, code, body, http.StatusConflict, "ticket_conflict", "")

	// An epic or label that exists is still refused to an unreachable ticket's caller:
	// carol is in another workspace, so the ticket itself is not found.
	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+made.ID, replace("t", map[string]any{"epic_id": epic.ID}), f.carolC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, "ticket_not_found", "")
}

func TestDeletingAParentLeavesTheTicketAndLosesTheLink(t *testing.T) {
	f := newFeed(t)
	epic, mile, bug, ux := f.epic(t, "Auth"), f.milestone(t, "1.0"), f.label(t, "bug"), f.label(t, "ux")
	made := f.tk(t, "t", map[string]any{"epic_id": epic.ID, "milestone_id": mile.ID, "label_ids": []string{bug.ID, ux.ID}})

	for _, path := range []string{"/epics/" + epic.ID, "/milestones/" + mile.ID, "/labels/" + bug.ID} {
		if rec := call(t, f.mux, http.MethodDelete, V1+path, nil, f.willC); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s: status %d - %.300s", path, rec.Code, rec.Body)
		}
	}

	got := f.hGet(t, made.ID)
	if got.EpicID != nil || got.MilestoneID != nil || !slices.Equal(got.LabelIDs, []string{ux.ID}) {
		t.Errorf("after the deletes, %+v, want no epic, no milestone and only ux", got)
	}
	// Not rewritten: a delete is announced by its own event, not by a ticket write.
	if got.Version != made.Version {
		t.Errorf("version %d after the deletes, want %d: the tickets are not rewritten", got.Version, made.Version)
	}
}

func TestSprintMembershipAcrossEpics(t *testing.T) {
	f := newFeed(t)
	epicA, epicB := f.epic(t, "A"), f.epic(t, "B")
	s1, s2 := f.sprint(t, "S1"), f.sprint(t, "S2")

	inA := f.tk(t, "in A", map[string]any{"epic_id": epicA.ID})
	inB := f.tk(t, "in B", map[string]any{"epic_id": epicB.ID})
	loose := f.tk(t, "no epic", nil)

	// Listening starts after the setup, so the first events are the memberships.
	sarah := f.open(t, f.cookie(f.sarahC))

	add := func(sprintID, ticketID string) int {
		return call(t, f.mux, http.MethodPost, V1+"/sprints/"+sprintID+"/tickets", map[string]any{"ticket_id": ticketID}, f.willC).Code
	}

	for _, tk := range []hTicket{inA, inB, loose} {
		if code := add(s1.ID, tk.ID); code != http.StatusNoContent {
			t.Fatalf("add %s: status %d, want 204", tk.Title, code)
		}
	}
	// A ticket may be in several sprints.
	if code := add(s2.ID, inA.ID); code != http.StatusNoContent {
		t.Fatalf("add to a second sprint: status %d", code)
	}

	// Membership is read through the list's sprint filter, alone and with epic.
	if got, want := f.matching(t, eq("sprint", s1.ID)), sorted(inA.ID, inB.ID, loose.ID); !slices.Equal(got, want) {
		t.Errorf("sprint 1 = %v, want %v", got, want)
	}
	if got := f.matching(t, eq("sprint", s2.ID)); !slices.Equal(got, []string{inA.ID}) {
		t.Errorf("sprint 2 = %v, want just the ticket from epic A", got)
	}
	if got := f.matching(t, eq("sprint", s1.ID), eq("epic", epicB.ID)); !slices.Equal(got, []string{inB.ID}) {
		t.Errorf("sprint 1 and epic B = %v", got)
	}

	// The epic did not move, and the ticket's version rose once per membership.
	if got := f.hGet(t, inA.ID); got.EpicID == nil || *got.EpicID != epicA.ID || got.Version != inA.Version+2 {
		t.Errorf("ticket in two sprints and an epic = %+v, want epic A and version %d", got, inA.Version+2)
	}

	// Joining is announced as ticket.updated, at the new version. (Three adds to s1, one to s2.)
	var last eventWire
	for range 4 {
		last = readEvent(t, sarah)
		if last.Type != "ticket.updated" {
			t.Fatalf("event %+v, want ticket.updated", last)
		}
	}
	if last.TicketID != inA.ID || last.Version != inA.Version+2 {
		t.Errorf("last event %+v, want the second join of ticket A at version %d", last, inA.Version+2)
	}

	// Adding again is a success that changes nothing and says nothing.
	if code := add(s1.ID, inA.ID); code != http.StatusNoContent {
		t.Errorf("re-adding: status %d, want 204", code)
	}
	if got := f.hGet(t, inA.ID); got.Version != inA.Version+2 {
		t.Errorf("version %d after a no-op add, want %d", got.Version, inA.Version+2)
	}

	// Remove: version rises, announced; removing a non-member is a quiet 204.
	rm := func(sprintID, ticketID string) int {
		return call(t, f.mux, http.MethodDelete, V1+"/sprints/"+sprintID+"/tickets/"+ticketID, nil, f.willC).Code
	}
	if code := rm(s1.ID, inB.ID); code != http.StatusNoContent {
		t.Fatalf("remove: status %d", code)
	}
	if ev := readEvent(t, sarah); ev.Type != "ticket.updated" || ev.TicketID != inB.ID || ev.Version != inB.Version+2 {
		t.Errorf("event %+v, want ticket.updated for B at version %d (join, then leave)", ev, inB.Version+2)
	}
	if code := rm(s1.ID, inB.ID); code != http.StatusNoContent {
		t.Errorf("removing a non-member: status %d, want 204", code)
	}
	if got, want := f.matching(t, eq("sprint", s1.ID)), sorted(inA.ID, loose.ID); !slices.Equal(got, want) {
		t.Errorf("sprint 1 after the removal = %v, want %v", got, want)
	}

	// Deleting the sprint drops its memberships and leaves the tickets.
	if rec := call(t, f.mux, http.MethodDelete, V1+"/sprints/"+s1.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("delete sprint: status %d", rec.Code)
	}
	if got := f.matching(t, eq("sprint", s1.ID)); len(got) != 0 {
		t.Errorf("a deleted sprint still matches %v", got)
	}
	if n := len(f.live(t, f.willC)); n != 3 {
		t.Errorf("%d tickets after deleting a sprint, want all 3", n)
	}
}

func TestOnlyATicketOfTheSprintsProjectCanJoin(t *testing.T) {
	f := newFeed(t)
	s := f.sprint(t, "S")
	proj2 := createProject(t, f.mux, f.willC, f.project.TeamID, "Second")
	rec := f.post(t, f.willC, map[string]any{"project_id": proj2.ID, "title": "elsewhere"})
	var foreign hTicket
	decode(t, rec, &foreign)
	local := f.tk(t, "local", nil)
	archived := f.tk(t, "gone", nil)
	if rec := call(t, f.mux, http.MethodDelete, V1+"/tickets/"+archived.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("archive: status %d", rec.Code)
	}

	join := func(cookie, sprintID, ticketID string) *struct {
		code int
		body []byte
	} {
		rec := call(t, f.mux, http.MethodPost, V1+"/sprints/"+sprintID+"/tickets", map[string]any{"ticket_id": ticketID}, cookie)

		return &struct {
			code int
			body []byte
		}{rec.Code, rec.Body.Bytes()}
	}

	for _, id := range []string{foreign.ID, archived.ID, "00000000-0000-0000-0000-0000000000aa"} {
		r := join(f.willC, s.ID, id)
		wantProblem(t, r.code, r.body, http.StatusUnprocessableEntity, "ticket_not_found", "body.ticket_id")
	}

	r := join(f.willC, "00000000-0000-0000-0000-0000000000aa", local.ID)
	wantProblem(t, r.code, r.body, http.StatusNotFound, "sprint_not_found", "")

	// Carol is in another workspace: the sprint is not found for her, and she cannot
	// reach the sprint's tickets by putting one of hers in it either.
	r = join(f.carolC, s.ID, local.ID)
	wantProblem(t, r.code, r.body, http.StatusNotFound, "sprint_not_found", "")
	rec = call(t, f.mux, http.MethodDelete, V1+"/sprints/"+s.ID+"/tickets/"+local.ID, nil, f.carolC)
	wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, "sprint_not_found", "")

	if got := f.matching(t, eq("sprint", s.ID)); len(got) != 0 {
		t.Errorf("sprint holds %v after only refusals", got)
	}
}

func TestHierarchyIsScopedToTheWorkspace(t *testing.T) {
	f := newFeed(t)
	e, m, s, l := f.epic(t, "e"), f.milestone(t, "m"), f.sprint(t, "s"), f.label(t, "l")

	for _, c := range []struct{ base, id, notFound string }{
		{"/epics", e.ID, "epic_not_found"}, {"/milestones", m.ID, "milestone_not_found"},
		{"/sprints", s.ID, "sprint_not_found"}, {"/labels", l.ID, "label_not_found"},
	} {
		for _, req := range []struct {
			method string
			body   any
		}{{http.MethodGet, nil}, {http.MethodPut, map[string]any{"name": "mine now"}}, {http.MethodDelete, nil}} {
			rec := call(t, f.mux, req.method, V1+c.base+"/"+c.id, req.body, f.carolC)
			wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, c.notFound, "")
		}
	}

	for _, c := range []struct {
		path string
		body map[string]any
		code string
	}{
		{"/epics", map[string]any{"project_id": f.project.ID, "name": "x"}, "project_not_found"},
		{"/epics/list", map[string]any{"project_id": f.project.ID}, "project_not_found"},
		{"/milestones", map[string]any{"project_id": f.project.ID, "name": "x"}, "project_not_found"},
		{"/milestones/list", map[string]any{"project_id": f.project.ID}, "project_not_found"},
		{"/sprints", map[string]any{"project_id": f.project.ID, "name": "x"}, "project_not_found"},
		{"/sprints/list", map[string]any{"project_id": f.project.ID}, "project_not_found"},
		{"/labels", map[string]any{"team_id": f.project.TeamID, "name": "x", "color": "x"}, "team_not_found"},
		{"/labels/list", map[string]any{"team_id": f.project.TeamID}, "team_not_found"},
	} {
		rec := call(t, f.mux, http.MethodPost, V1+c.path, c.body, f.carolC)
		wantProblem(t, rec.Code, rec.Body.Bytes(), http.StatusNotFound, c.code, "")
	}

	// And nothing of carol's refusals changed anything.
	rec := call(t, f.mux, http.MethodGet, V1+"/epics/"+e.ID, nil, f.willC)
	var got hParent
	decode(t, rec, &got)
	if got.Name != "e" {
		t.Errorf("epic name %q after carol's refused rename, want e", got.Name)
	}
}

func TestHierarchyWritesAreAnnounced(t *testing.T) {
	f := newFeed(t)
	sarah := f.open(t, f.cookie(f.sarahC))
	carol := f.open(t, f.cookie(f.carolC))

	e := f.epic(t, "Auth")
	if ev := readEvent(t, sarah); ev.Type != "epic.created" || ev.EpicID != e.ID || ev.ProjectID != f.project.ID {
		t.Errorf("event %+v, want epic.created for %s", ev, e.ID)
	}
	call(t, f.mux, http.MethodPut, V1+"/epics/"+e.ID, map[string]any{"name": "Auth 2"}, f.willC)
	if ev := readEvent(t, sarah); ev.Type != "epic.updated" || ev.EpicID != e.ID {
		t.Errorf("event %+v, want epic.updated", ev)
	}
	call(t, f.mux, http.MethodDelete, V1+"/epics/"+e.ID, nil, f.willC)
	if ev := readEvent(t, sarah); ev.Type != "epic.deleted" || ev.EpicID != e.ID {
		t.Errorf("event %+v, want epic.deleted", ev)
	}

	for _, c := range []struct {
		make           func() hParent
		path, kind, id string
	}{
		{func() hParent { return f.sprint(t, "S") }, "/sprints/", "sprint", ""},
		{func() hParent { return f.milestone(t, "M") }, "/milestones/", "milestone", ""},
		{func() hParent { return f.label(t, "L") }, "/labels/", "label", ""},
	} {
		made := c.make()
		if ev := readEvent(t, sarah); ev.Type != c.kind+".created" {
			t.Errorf("event %+v, want %s.created", ev, c.kind)
		}
		call(t, f.mux, http.MethodDelete, V1+c.path+made.ID, nil, f.willC)
		ev := readEvent(t, sarah)
		if ev.Type != c.kind+".deleted" {
			t.Errorf("event %+v, want %s.deleted", ev, c.kind)
		}
		if body := ev.Epic; body != nil {
			t.Errorf("a %s event carries an epic %v", c.kind, body)
		}
	}

	// A ticket create that names parents is the ordinary ticket.created, with them in it.
	m := f.milestone(t, "1.0")
	readEvent(t, sarah)
	l := f.label(t, "bug")
	readEvent(t, sarah)
	made := f.tk(t, "t", map[string]any{"milestone_id": m.ID, "label_ids": []string{l.ID}})
	ev := readEvent(t, sarah)
	if ev.Type != "ticket.created" || ev.TicketID != made.ID || ev.Ticket["milestone_id"] != m.ID {
		t.Errorf("event %+v, want ticket.created carrying the milestone", ev)
	}
	if ids, _ := ev.Ticket["label_ids"].([]any); len(ids) != 1 || ids[0] != l.ID {
		t.Errorf("event ticket label_ids = %v, want [%s]", ev.Ticket["label_ids"], l.ID)
	}

	// Carol is in another workspace and heard none of it: the first thing she reads is
	// her own epic.
	team := createTeam(t, f.mux, f.carolC, f.carol.WorkspaceID, "Elsewhere")
	hers := createProject(t, f.mux, f.carolC, team.ID, "Other")
	rec := call(t, f.mux, http.MethodPost, V1+"/epics", map[string]any{"project_id": hers.ID, "name": "hers"}, f.carolC)
	var herEpic hParent
	decode(t, rec, &herEpic)
	if ev := readEvent(t, carol); ev.Type != "epic.created" || ev.EpicID != herEpic.ID {
		t.Errorf("carol's first event %+v, want her own epic.created", ev)
	}
}

func TestTheOpenAPIDocumentCarriesTheHierarchy(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()
	body := doc.Components.Schemas.Map()["UpdateTicketInputBody"]
	if body == nil {
		t.Fatal("no UpdateTicketInputBody")
	}

	// The three new members are optional: a client that has never heard of them still
	// sends a valid body.
	for _, name := range []string{"epic_id", "milestone_id", "label_ids"} {
		if body.Properties[name] == nil {
			t.Errorf("update body has no %s", name)
		}
		if slices.Contains(body.Required, name) {
			t.Errorf("%s is required on update", name)
		}
	}
	if strings.Contains(strings.Join(body.Required, ","), "epic") {
		t.Errorf("required = %v", body.Required)
	}
}

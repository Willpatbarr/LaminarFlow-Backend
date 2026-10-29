package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// LAM-89. The activity log over the wire: what each write records, that a body-only save
// records nothing, newest first, the 422 and the 404, and the change feed.

type activityWire struct {
	ID            string    `json:"id"`
	EntityType    string    `json:"entity_type"`
	EntityID      string    `json:"entity_id"`
	ActorID       *string   `json:"actor_id"`
	Kind          string    `json:"kind"`
	Field         *string   `json:"field"`
	SummaryBefore *string   `json:"summary_before"`
	SummaryAfter  *string   `json:"summary_after"`
	CreatedAt     time.Time `json:"created_at"`
}

func entity(kind, id string, extra ...map[string]any) map[string]any {
	conds := []map[string]any{
		{"field": "entity_type", "op": "eq", "value": kind},
		{"field": "entity_id", "op": "eq", "value": id},
	}
	conds = append(conds, extra...)

	return map[string]any{"op": "and", "conditions": conds}
}

func (f feed) history(t *testing.T, cookie, kind, id string) []activityWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity(kind, id)}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list activity: %d %.300s", rec.Code, rec.Body)
	}
	var out struct {
		Items []activityWire `json:"items"`
	}
	decode(t, rec, &out)

	return out.Items
}

// kinds is the log as "kind" or "kind:field" strings, oldest first, for a short assertion.
func kinds(items []activityWire) []string {
	out := make([]string, len(items))
	for i := range items {
		it := items[len(items)-1-i]
		out[i] = it.Kind
		if it.Field != nil {
			out[i] += ":" + *it.Field
		}
	}

	return out
}

func want(t *testing.T, got []activityWire, expect ...string) {
	t.Helper()

	have := strings.Join(kinds(got), ",")
	if have != strings.Join(expect, ",") {
		t.Errorf("history (oldest first) = [%s], want [%s]", have, strings.Join(expect, ","))
	}
}

func TestACreateAndAStatusChangeEachMakeExactlyTheirRows(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses
	tk := f.ticketWith(t, "Fix login", "")

	got := f.history(t, f.sarahC, "ticket", tk.ID)
	want(t, got, "created")
	if got[0].ActorID == nil || *got[0].ActorID != f.will.AccountID || got[0].EntityType != "ticket" || got[0].EntityID != tk.ID ||
		got[0].SummaryBefore != nil || got[0].SummaryAfter != nil || got[0].CreatedAt.IsZero() {
		t.Errorf("created entry = %+v", got[0])
	}

	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+tk.ID,
		replace("Fix login", map[string]any{"description": "", "status_id": statuses[1].ID}), f.sarahC)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	got = f.history(t, f.willC, "ticket", tk.ID)
	want(t, got, "created", "status_changed")
	if got[0].Kind != "status_changed" || got[0].SummaryBefore != nil || got[0].SummaryAfter == nil || *got[0].SummaryAfter != statuses[1].ID ||
		*got[0].ActorID != f.sarah.AccountID {
		t.Errorf("status entry = %+v, want newest first, by sarah, null -> %s", got[0], statuses[1].ID)
	}

	// The same PUT again changes nothing, so it is not history.
	call(t, f.mux, http.MethodPut, V1+"/tickets/"+tk.ID,
		replace("Fix login", map[string]any{"description": "", "status_id": statuses[1].ID}), f.sarahC)
	want(t, f.history(t, f.willC, "ticket", tk.ID), "created", "status_changed")
}

func TestATicketEditIsRecordedFieldByField(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses
	epic := f.epic(t, "Auth")
	ms := f.milestone(t, "1.0")
	l1, l2 := f.label(t, "bug"), f.label(t, "ux")
	tk := f.tk(t, "Old title", map[string]any{"label_ids": []string{l1.ID}})

	rec := call(t, f.mux, http.MethodPut, V1+"/tickets/"+tk.ID, map[string]any{
		"title": "New title", "description": "written", "status_id": statuses[0].ID, "assignee_id": f.sarah.AccountID,
		"epic_id": epic.ID, "milestone_id": ms.ID, "label_ids": []string{l2.ID},
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}

	got := f.history(t, f.willC, "ticket", tk.ID)
	// One write, so one created_at; the order within it is by id, so compare as a set.
	have := map[string]int{}
	for _, it := range got[:len(got)-1] {
		k := it.Kind
		if it.Field != nil {
			k += ":" + *it.Field
		}
		have[k]++
	}
	for _, k := range []string{"field_changed:title", "field_changed:description", "status_changed", "assignee_changed",
		"parent_changed", "field_changed:milestone", "label_added", "label_removed"} {
		if have[k] != 1 {
			t.Errorf("%s recorded %d times, want 1 (have %v)", k, have[k], have)
		}
	}
	if len(got) != 9 || got[len(got)-1].Kind != "created" {
		t.Errorf("history has %d entries ending in %q, want 9 ending in created", len(got), got[len(got)-1].Kind)
	}
	for _, it := range got {
		switch {
		case it.Kind == "field_changed" && *it.Field == "title":
			if *it.SummaryBefore != "Old title" || *it.SummaryAfter != "New title" {
				t.Errorf("title entry = %+v", it)
			}
		case it.Kind == "label_added" && (it.SummaryBefore != nil || *it.SummaryAfter != l2.ID):
			t.Errorf("label_added = %+v", it)
		case it.Kind == "label_removed" && (it.SummaryAfter != nil || *it.SummaryBefore != l1.ID):
			t.Errorf("label_removed = %+v", it)
		case it.Kind == "parent_changed" && (it.SummaryBefore != nil || *it.SummaryAfter != epic.ID):
			t.Errorf("parent_changed = %+v", it)
		}
	}
}

func TestADragToAnotherColumnIsAStatusChangeAndAnArchiveIsRecorded(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses
	tk := f.ticketWith(t, "Card", "")

	if code, body := f.move(t, tk.ID, map[string]any{"status_id": statuses[2].ID}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, body)
	}
	// Dropped in the same column it is already in: a position change is not history.
	if code, body := f.move(t, tk.ID, map[string]any{"status_id": statuses[2].ID}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, body)
	}
	if rec := call(t, f.mux, http.MethodDelete, V1+"/tickets/"+tk.ID, nil, f.willC); rec.Code >= 300 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}

	// An archived ticket is not visible, so neither is its history.
	rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity("ticket", tk.ID)}, f.willC)
	if rec.Code != http.StatusNotFound {
		t.Errorf("history of an archived ticket: %d, want 404", rec.Code)
	}

	if rec := call(t, f.mux, http.MethodPost, V1+"/tickets/"+tk.ID+"/unarchive", nil, f.willC); rec.Code >= 300 {
		t.Fatalf("unarchive: %d %s", rec.Code, rec.Body)
	}
	want(t, f.history(t, f.willC, "ticket", tk.ID), "created", "status_changed", "archived", "unarchived")
}

func TestADocumentRecordsCoarseChangesAndNotABodySave(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Spec", "project_id": f.project.ID, "body": map[string]any{"content": "v1"}})

	put := func(title, typ string, aspect any, body map[string]any) {
		t.Helper()
		rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+doc.ID,
			map[string]any{"title": title, "type": typ, "aspect_type_id": aspect, "body": body}, f.sarahC)
		if rec.Code != http.StatusOK {
			t.Fatalf("put document: %d %s", rec.Code, rec.Body)
		}
	}

	put("Spec", "normal", nil, map[string]any{"content": "v2, only the body changed"})
	want(t, f.history(t, f.willC, "document", doc.ID), "created")

	put("Spec v2", "normal", nil, map[string]any{"content": "v2, only the body changed"})
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Risks")
	put("Spec v2", "aspect", typeID, map[string]any{fields[1]: "r"})
	// The conversion moved the old text into the first field, so both fields have a value now.
	// Same type, but one field lost its value: the set of fields changed.
	put("Spec v2", "aspect", typeID, map[string]any{fields[0]: "s"})
	// Same type, same fields, other text: a body save.
	put("Spec v2", "aspect", typeID, map[string]any{fields[0]: "s2"})

	got := f.history(t, f.willC, "document", doc.ID)
	want(t, got, "created", "field_changed:title", "aspect_type_changed", "field_changed:fields")
	// Newest first: fields, type, title, created.
	if got[1].Kind != "aspect_type_changed" || got[1].SummaryBefore != nil || *got[1].SummaryAfter != typeID {
		t.Errorf("type entry = %+v", got[1])
	}
	if got[2].Kind != "field_changed" || *got[2].SummaryBefore != "Spec" || *got[2].SummaryAfter != "Spec v2" {
		t.Errorf("title entry = %+v", got[2])
	}
	if got[0].SummaryBefore == nil || got[0].SummaryAfter == nil || *got[0].SummaryAfter != fields[0] {
		t.Errorf("fields entry = %+v, want the field ids before and after", got[0])
	}

	if rec := call(t, f.mux, http.MethodDelete, V1+"/documents/"+doc.ID, nil, f.willC); rec.Code >= 300 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/documents/"+doc.ID+"/unarchive", nil, f.willC); rec.Code >= 300 {
		t.Fatalf("unarchive: %d %s", rec.Code, rec.Body)
	}
	got = f.history(t, f.willC, "document", doc.ID)
	if n := len(got); n != 6 || got[0].Kind != "unarchived" || got[1].Kind != "archived" {
		t.Errorf("history = %v, want ending archived, unarchived", kinds(got))
	}
}

func TestTheEntityFilterIsRequiredAndScoped(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Private", "")

	for name, filter := range map[string]any{
		"no filter":  nil,
		"no entity":  map[string]any{"op": "and", "conditions": []map[string]any{{"field": "kind", "op": "eq", "value": "created"}}},
		"or":         map[string]any{"op": "or", "conditions": []map[string]any{{"field": "entity_type", "op": "eq", "value": "ticket"}, {"field": "entity_id", "op": "eq", "value": tk.ID}}},
		"nested":     map[string]any{"op": "and", "groups": []any{entity("ticket", tk.ID)}},
		"bad type":   entity("sprint", tk.ID),
		"bad id":     entity("ticket", "not-a-uuid"),
		"neq not eq": map[string]any{"op": "and", "conditions": []map[string]any{{"field": "entity_type", "op": "eq", "value": "ticket"}, {"field": "entity_id", "op": "neq", "value": tk.ID}}},
	} {
		body := map[string]any{}
		if filter != nil {
			body["filter"] = filter
		}
		rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", body, f.willC)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %.200s, want 422", name, rec.Code, rec.Body)
			continue
		}
		var p problemWire
		decode(t, rec, &p)
		if len(p.Errors) == 0 || !strings.HasPrefix(p.Errors[0].Location, "body.filter.entity_") {
			t.Errorf("%s: errors = %+v, want a location under body.filter.entity_*", name, p.Errors)
		}
	}

	// Only entity_type missing: the location names it, and only it.
	rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": map[string]any{"op": "and",
		"conditions": []map[string]any{{"field": "entity_id", "op": "eq", "value": tk.ID}}}}, f.willC)
	var p problemWire
	decode(t, rec, &p)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.filter.entity_type" {
		t.Errorf("errors = %+v, want exactly body.filter.entity_type", p.Errors)
	}

	// Another workspace's ticket, and one that does not exist, are the same 404.
	for _, id := range []string{tk.ID, "11111111-1111-4111-8111-111111111111"} {
		if rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity("ticket", id)}, f.carolC); rec.Code != http.StatusNotFound {
			t.Errorf("history of %s for an outsider: %d, want 404", id, rec.Code)
		}
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity("document", tk.ID)}, f.willC); rec.Code != http.StatusNotFound {
		t.Errorf("a ticket id as a document: %d, want 404", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity("ticket", tk.ID)}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: %d, want 401", rec.Code)
	}
}

func TestTheHistoryPagesNewestFirstAndFilters(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses
	tk := f.ticketWith(t, "Busy", "")
	for i := 0; i < 3; i++ {
		call(t, f.mux, http.MethodPut, V1+"/tickets/"+tk.ID,
			replace("Busy", map[string]any{"description": "", "status_id": statuses[i].ID}), f.willC)
	}
	all := f.history(t, f.willC, "ticket", tk.ID)
	want(t, all, "created", "status_changed", "status_changed", "status_changed")

	rec := call(t, f.mux, http.MethodPost, V1+"/activity/list", map[string]any{"filter": entity("ticket", tk.ID), "limit": 3}, f.willC)
	var page struct {
		Items      []activityWire `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		HasMore    bool           `json:"has_more"`
	}
	decode(t, rec, &page)
	if len(page.Items) != 3 || !page.HasMore || page.NextCursor == nil || page.Items[0].ID != all[0].ID {
		t.Fatalf("first page = %+v", page)
	}
	rec = call(t, f.mux, http.MethodPost, V1+"/activity/list",
		map[string]any{"filter": entity("ticket", tk.ID), "limit": 3, "cursor": *page.NextCursor}, f.willC)
	var rest struct {
		Items   []activityWire `json:"items"`
		HasMore bool           `json:"has_more"`
	}
	decode(t, rec, &rest)
	if len(rest.Items) != 1 || rest.Items[0].Kind != "created" || rest.HasMore {
		t.Errorf("second page = %+v", rest)
	}

	rec = call(t, f.mux, http.MethodPost, V1+"/activity/list",
		map[string]any{"filter": entity("ticket", tk.ID, map[string]any{"field": "kind", "op": "eq", "value": "created"})}, f.willC)
	var only struct {
		Items []activityWire `json:"items"`
	}
	decode(t, rec, &only)
	if len(only.Items) != 1 || only.Items[0].Kind != "created" {
		t.Errorf("kind filter = %+v", only.Items)
	}
}

func TestAnEntryIsOnTheChangeFeedAfterItsWrite(t *testing.T) {
	f := newFeed(t)
	statuses := f.team(t).Statuses
	tk := f.ticketWith(t, "Watched", "")
	watcher := f.open(t, f.cookie(f.sarahC))

	call(t, f.mux, http.MethodPut, V1+"/tickets/"+tk.ID,
		replace("Watched", map[string]any{"description": "", "status_id": statuses[1].ID}), f.willC)

	if ev := readEventAny(t, watcher); ev.Type != "ticket.updated" || ev.TicketID != tk.ID {
		t.Fatalf("first event = %+v, want ticket.updated", ev)
	}
	ev := readEventAny(t, watcher)
	if ev.Type != "activity.created" || ev.TicketID != tk.ID || ev.DocumentID != "" || ev.ActorID != f.will.AccountID ||
		ev.Activity["kind"] != "status_changed" || ev.Activity["entity_type"] != "ticket" || ev.Activity["entity_id"] != tk.ID ||
		ev.ActivityID == "" || ev.Activity["id"] != ev.ActivityID || ev.Version != 0 || ev.Ticket != nil {
		t.Errorf("activity event = %+v", ev)
	}
	if got := f.history(t, f.willC, "ticket", tk.ID); got[0].ID != ev.ActivityID {
		t.Errorf("event names %s, newest entry is %s", ev.ActivityID, got[0].ID)
	}

	doc := f.document(t, f.willC, map[string]any{"title": "Doc", "project_id": f.project.ID})
	if ev := readEventAny(t, watcher); ev.Type != "document.created" || ev.DocumentID != doc.ID {
		t.Fatalf("event = %+v, want document.created", ev)
	}
	if ev := readEventAny(t, watcher); ev.Type != "activity.created" || ev.DocumentID != doc.ID || ev.TicketID != "" || ev.Activity["kind"] != "created" {
		t.Errorf("document activity event = %+v", ev)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// LAM-99. The comments API over the wire: create on a ticket and on a document, the
// authorless comment, author-only edit and delete, the 422s, search staying live, the change
// feed, and Idempotency-Key.

type commentWire struct {
	ID           string     `json:"id"`
	TicketID     *string    `json:"ticket_id"`
	DocumentID   *string    `json:"document_id"`
	FieldID      *string    `json:"field_id"`
	RegionStart  *int       `json:"region_start"`
	RegionEnd    *int       `json:"region_end"`
	AuthorID     *string    `json:"author_id"`
	Body         string     `json:"body"`
	ReviewStatus *string    `json:"review_status"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	EditedAt     *time.Time `json:"edited_at"`
}

func (f feed) postComment(t *testing.T, cookie string, body map[string]any, header ...string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/comments", body, cookie, header...)
}

func (f feed) comment(t *testing.T, cookie string, body map[string]any) commentWire {
	t.Helper()

	rec := f.postComment(t, cookie, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create comment %v: %d %.300s", body, rec.Code, rec.Body)
	}
	var c commentWire
	decode(t, rec, &c)

	return c
}

func (f feed) comments(t *testing.T, cookie string, filter map[string]any) []commentWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/comments/list", map[string]any{"filter": filter}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list comments: %d %.300s", rec.Code, rec.Body)
	}
	var out struct {
		Items []commentWire `json:"items"`
	}
	decode(t, rec, &out)

	return out.Items
}

func byTicket(id string) map[string]any {
	return map[string]any{"op": "and", "conditions": []map[string]any{{"field": "ticket_id", "op": "eq", "value": id}}}
}

func byDocument(id string) map[string]any {
	return map[string]any{"op": "and", "conditions": []map[string]any{{"field": "document_id", "op": "eq", "value": id}}}
}

func TestACommentOnATicketAndOnADocument(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Fix login", "")
	doc := f.document(t, f.willC, map[string]any{"title": "Spec", "project_id": f.project.ID})

	c := f.comment(t, f.sarahC, map[string]any{"ticket_id": tk.ID, "body": "on the ticket"})
	if c.ID == "" || c.TicketID == nil || *c.TicketID != tk.ID || c.DocumentID != nil || c.FieldID != nil ||
		c.RegionStart != nil || c.RegionEnd != nil || c.ReviewStatus != nil ||
		c.AuthorID == nil || *c.AuthorID != f.sarah.AccountID || c.Body != "on the ticket" {
		t.Errorf("ticket comment = %+v", c)
	}

	d := f.comment(t, f.willC, map[string]any{"document_id": doc.ID, "body": "on the doc", "region_start": 3, "region_end": 3})
	if d.DocumentID == nil || *d.DocumentID != doc.ID || d.TicketID != nil ||
		d.RegionStart == nil || *d.RegionStart != 3 || d.RegionEnd == nil || *d.RegionEnd != 3 {
		t.Errorf("document comment = %+v (a zero-length caret is a legal region)", d)
	}

	if got := f.comments(t, f.willC, byTicket(tk.ID)); len(got) != 1 || got[0].ID != c.ID {
		t.Errorf("ticket thread = %+v", got)
	}
	if got := f.comments(t, f.willC, byDocument(doc.ID)); len(got) != 1 || got[0].ID != d.ID {
		t.Errorf("document thread = %+v", got)
	}
}

func TestAThreadListsOldestFirstAndPages(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Thread", "")
	var want []string
	for _, b := range []string{"one", "two", "three"} {
		want = append(want, f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": b}).ID)
	}

	got := f.comments(t, f.sarahC, byTicket(tk.ID))
	if len(got) != 3 || got[0].ID != want[0] || got[1].ID != want[1] || got[2].ID != want[2] {
		t.Fatalf("thread = %+v, want oldest first %v", got, want)
	}

	rec := call(t, f.mux, http.MethodPost, V1+"/comments/list", map[string]any{"filter": byTicket(tk.ID), "limit": 2}, f.willC)
	var page struct {
		Items      []commentWire `json:"items"`
		NextCursor *string       `json:"next_cursor"`
		HasMore    bool          `json:"has_more"`
	}
	decode(t, rec, &page)
	if len(page.Items) != 2 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("first page = %+v", page)
	}
	rec = call(t, f.mux, http.MethodPost, V1+"/comments/list",
		map[string]any{"filter": byTicket(tk.ID), "limit": 2, "cursor": *page.NextCursor}, f.willC)
	var rest struct {
		Items   []commentWire `json:"items"`
		HasMore bool          `json:"has_more"`
	}
	decode(t, rec, &rest)
	if len(rest.Items) != 1 || rest.Items[0].ID != want[2] || rest.HasMore {
		t.Errorf("second page = %+v", rest)
	}
}

func TestACommentWhoseAuthorWasDeletedIsAuthorless(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Orphans", "")
	c := f.comment(t, f.sarahC, map[string]any{"ticket_id": tk.ID, "body": "from sarah"})

	if rec := call(t, f.mux, http.MethodDelete, V1+"/accounts/me", nil, f.sarahC); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("delete account: %d %s", rec.Code, rec.Body)
	}

	got := f.comments(t, f.willC, byTicket(tk.ID))
	if len(got) != 1 || got[0].ID != c.ID || got[0].AuthorID != nil || got[0].Body != "from sarah" {
		t.Fatalf("thread = %+v, want the comment kept with a null author_id", got)
	}

	// Nobody wrote it any more, so nobody can change it.
	rec := call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "mine now"}, f.willC)
	wantDocProblem(t, rec, http.StatusForbidden, "comment_not_author", "")
}

func TestOnlyTheAuthorEditsOrDeletes(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Mine", "")
	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "original"})

	rec := call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "hijacked"}, f.sarahC)
	wantDocProblem(t, rec, http.StatusForbidden, "comment_not_author", "")
	rec = call(t, f.mux, http.MethodDelete, V1+"/comments/"+c.ID, nil, f.sarahC)
	wantDocProblem(t, rec, http.StatusForbidden, "comment_not_author", "")

	if got := f.comments(t, f.willC, byTicket(tk.ID)); len(got) != 1 || got[0].Body != "original" {
		t.Fatalf("thread after refused writes = %+v", got)
	}

	rec = call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "edited"}, f.willC)
	var edited commentWire
	decode(t, rec, &edited)
	if c.EditedAt != nil || edited.EditedAt == nil {
		t.Errorf("edited_at before %v, after %v; want nil then set", c.EditedAt, edited.EditedAt)
	}
	if rec.Code != http.StatusOK || edited.Body != "edited" || edited.ID != c.ID || !edited.UpdatedAt.After(c.UpdatedAt) ||
		!edited.CreatedAt.Equal(c.CreatedAt) || edited.TicketID == nil || *edited.TicketID != *c.TicketID {
		t.Errorf("edit = %d %+v", rec.Code, edited)
	}

	if rec = call(t, f.mux, http.MethodDelete, V1+"/comments/"+c.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, f.mux, http.MethodDelete, V1+"/comments/"+c.ID, nil, f.willC)
	wantDocProblem(t, rec, http.StatusNotFound, "comment_not_found", "")
	rec = call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "x"}, f.willC)
	wantDocProblem(t, rec, http.StatusNotFound, "comment_not_found", "")
}

func TestCommentsAreInvisibleOutsideTheWorkspace(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Private", "")
	doc := f.document(t, f.willC, map[string]any{"title": "Private doc"})
	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "secret"})

	for _, target := range []map[string]any{{"ticket_id": tk.ID}, {"document_id": doc.ID}} {
		target["body"] = "intruder"
		rec := f.postComment(t, f.carolC, target)
		wantDocProblem(t, rec, http.StatusNotFound, "not_found", "")
	}
	rec := call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "x"}, f.carolC)
	wantDocProblem(t, rec, http.StatusNotFound, "comment_not_found", "")
	rec = call(t, f.mux, http.MethodDelete, V1+"/comments/"+c.ID, nil, f.carolC)
	wantDocProblem(t, rec, http.StatusNotFound, "comment_not_found", "")
	if got := f.comments(t, f.carolC, byTicket(tk.ID)); len(got) != 0 {
		t.Errorf("another workspace lists %+v", got)
	}
}

func TestACommentOnAnArchivedParentIsNotFound(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Going", "")
	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "before"})
	call(t, f.mux, http.MethodDelete, V1+"/tickets/"+tk.ID, nil, f.willC)

	wantDocProblem(t, f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "after"}), http.StatusNotFound, "not_found", "")
	wantDocProblem(t, call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "x"}, f.willC),
		http.StatusNotFound, "comment_not_found", "")
	if got := f.comments(t, f.willC, byTicket(tk.ID)); len(got) != 0 {
		t.Errorf("archived ticket's thread = %+v", got)
	}

	call(t, f.mux, http.MethodPost, V1+"/tickets/"+tk.ID+"/unarchive", nil, f.willC, "If-Match", `"2"`)
	if got := f.comments(t, f.willC, byTicket(tk.ID)); len(got) != 1 {
		t.Errorf("restored ticket's thread = %+v, want the comment back", got)
	}
}

func TestACommentIsRefusedWithTheFieldNamed(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "T", "")
	typeID, fields := f.aspectType(t, f.willC, "Summary")
	_, otherFields := f.aspectType(t, f.willC, "Other")
	aspect := f.document(t, f.willC, map[string]any{"title": "A", "type": "aspect", "aspect_type_id": typeID, "project_id": f.project.ID})
	plain := f.document(t, f.willC, map[string]any{"title": "P", "project_id": f.project.ID})

	for _, tc := range []struct {
		name     string
		body     map[string]any
		location string
	}{
		{"field on a ticket", map[string]any{"ticket_id": tk.ID, "body": "x", "field_id": fields[0]}, "body.field_id"},
		{"field on a normal document", map[string]any{"document_id": plain.ID, "body": "x", "field_id": fields[0]}, "body.field_id"},
		{"field of another type", map[string]any{"document_id": aspect.ID, "body": "x", "field_id": otherFields[0]}, "body.field_id"},
		{"no target", map[string]any{"body": "x"}, "body"},
		{"two targets", map[string]any{"ticket_id": tk.ID, "document_id": plain.ID, "body": "x"}, "body"},
		{"half a region", map[string]any{"ticket_id": tk.ID, "body": "x", "region_start": 1}, "body.region_end"},
		{"half a region, the other half", map[string]any{"ticket_id": tk.ID, "body": "x", "region_end": 1}, "body.region_start"},
		{"region backwards", map[string]any{"ticket_id": tk.ID, "body": "x", "region_start": 5, "region_end": 4}, "body.region_end"},
		{"negative region", map[string]any{"ticket_id": tk.ID, "body": "x", "region_start": -1, "region_end": 4}, "body.region_start"},
		{"blank body", map[string]any{"ticket_id": tk.ID, "body": "   "}, "body.body"},
		{"empty body", map[string]any{"ticket_id": tk.ID, "body": ""}, "body.body"},
		{"over-long body", map[string]any{"ticket_id": tk.ID, "body": strings.Repeat("é", 50001)}, "body.body"},
		{"review_status is not accepted here", map[string]any{"ticket_id": tk.ID, "body": "x", "review_status": "pending"}, ""},
	} {
		rec := f.postComment(t, f.willC, tc.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %.200s, want 422", tc.name, rec.Code, rec.Body)
			continue
		}
		if tc.location != "" {
			wantDocProblem(t, rec, http.StatusUnprocessableEntity, "unprocessable_entity", tc.location)
		}
	}
	if got := f.comments(t, f.willC, byTicket(tk.ID)); len(got) != 0 {
		t.Errorf("refused creates left %+v", got)
	}

	// The same shapes are fine when right: a field of the document's own type, and a long-but-legal body.
	ok := f.comment(t, f.willC, map[string]any{"document_id": aspect.ID, "body": strings.Repeat("é", 50000), "field_id": fields[0], "region_start": 0, "region_end": 4})
	if ok.FieldID == nil || *ok.FieldID != fields[0] {
		t.Errorf("anchored comment = %+v", ok)
	}
}

func TestEditingBodyOnlyAndRefusingABlankEdit(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "E", "")
	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "orig"})

	wantDocProblem(t, call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": " "}, f.willC),
		http.StatusUnprocessableEntity, "unprocessable_entity", "body.body")
	rec := call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "x", "ticket_id": tk.ID}, f.willC)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("edit naming another member: %d, want 422 - an edit changes body only", rec.Code)
	}
}

func TestSearchStaysLiveAcrossCreateEditAndDelete(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Parent ticket", "")
	doc := f.document(t, f.willC, map[string]any{"title": "Parent doc", "project_id": f.project.ID})

	tc := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "spotted an ocelot"})
	dc := f.comment(t, f.willC, map[string]any{"document_id": doc.ID, "body": "spotted a lynx"})

	for word, want := range map[string][2]string{"ocelot": {"ticket", tk.ID}, "lynx": {"document", doc.ID}} {
		hits := f.search(t, f.sarahC, map[string]any{"q": word})
		if len(hits) != 1 || hits[0].Type != "comment" || hits[0].ParentType == nil || *hits[0].ParentType != want[0] || *hits[0].ParentID != want[1] {
			t.Fatalf("%s: hits = %+v, want one comment on %v", word, hits, want)
		}
	}

	call(t, f.mux, http.MethodPut, V1+"/comments/"+tc.ID, map[string]any{"body": "spotted a margay"}, f.willC)
	if hits := f.search(t, f.willC, map[string]any{"q": "ocelot"}); len(hits) != 0 {
		t.Errorf("old text still found after an edit: %+v", hits)
	}
	if hits := f.search(t, f.willC, map[string]any{"q": "margay"}); len(hits) != 1 || hits[0].Type != "comment" {
		t.Errorf("new text not found after an edit: %+v", hits)
	}

	call(t, f.mux, http.MethodDelete, V1+"/comments/"+dc.ID, nil, f.willC)
	if hits := f.search(t, f.willC, map[string]any{"q": "lynx"}); len(hits) != 0 {
		t.Errorf("deleted comment still found: %+v", hits)
	}
}

func TestCommentWritesAreOnTheChangeFeed(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Watched", "")
	watcher := f.open(t, f.cookie(f.sarahC))

	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "hello"})
	ev := readEvent(t, watcher)
	if ev.Type != "comment.created" || ev.CommentID != c.ID || ev.TicketID != tk.ID || ev.ActorID != f.will.AccountID ||
		ev.Comment["body"] != "hello" || ev.Comment["id"] != c.ID {
		t.Errorf("created event = %+v", ev)
	}

	// A refused write publishes nothing.
	f.postComment(t, f.carolC, map[string]any{"ticket_id": tk.ID, "body": "no"})
	call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "no"}, f.sarahC)

	call(t, f.mux, http.MethodPut, V1+"/comments/"+c.ID, map[string]any{"body": "hello again"}, f.willC)
	if ev = readEvent(t, watcher); ev.Type != "comment.updated" || ev.CommentID != c.ID || ev.Comment["body"] != "hello again" {
		t.Errorf("updated event = %+v", ev)
	}

	call(t, f.mux, http.MethodDelete, V1+"/comments/"+c.ID, nil, f.willC)
	if ev = readEvent(t, watcher); ev.Type != "comment.deleted" || ev.CommentID != c.ID || ev.Comment["body"] != "hello again" {
		t.Errorf("deleted event = %+v, want the comment as it was", ev)
	}

	// A document comment names its document.
	doc := f.document(t, f.willC, map[string]any{"title": "D"})
	readEvent(t, watcher) // document.created
	dc := f.comment(t, f.willC, map[string]any{"document_id": doc.ID, "body": "on doc"})
	if ev = readEvent(t, watcher); ev.Type != "comment.created" || ev.DocumentID != doc.ID || ev.TicketID != "" || ev.CommentID != dc.ID {
		t.Errorf("document comment event = %+v", ev)
	}
}

func TestTheSameKeyTwiceIsOneCommentAndOneEvent(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Retry", "")
	watcher := f.open(t, f.cookie(f.sarahC))
	body := map[string]any{"ticket_id": tk.ID, "body": "a pangolin"}

	first := f.postComment(t, f.willC, body, "Idempotency-Key", "c-1")
	second := f.postComment(t, f.willC, body, "Idempotency-Key", "c-1")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d, want 200 for both", first.Code, second.Code)
	}
	if first.Header().Get("Idempotent-Replayed") != "false" || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("Idempotent-Replayed = %q then %q", first.Header().Get("Idempotent-Replayed"), second.Header().Get("Idempotent-Replayed"))
	}
	var a, b commentWire
	decode(t, first, &a)
	decode(t, second, &b)
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("ids %q and %q, want the same", a.ID, b.ID)
	}
	if n := len(f.comments(t, f.willC, byTicket(tk.ID))); n != 1 {
		t.Errorf("%d comments, want 1", n)
	}

	// Exactly one comment.created: the marker is the very next frame.
	if ev := readEvent(t, watcher); ev.Type != "comment.created" || ev.CommentID != a.ID {
		t.Errorf("first frame = %+v", ev)
	}
	marker := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "marker"})
	if ev := readEvent(t, watcher); ev.CommentID != marker.ID {
		t.Errorf("second frame = %+v, want the marker, not a replay", ev)
	}

	if hits := f.search(t, f.willC, map[string]any{"q": "pangolin"}); len(hits) != 1 {
		t.Errorf("%d search hits for the replayed comment, want 1", len(hits))
	}
}

func TestAReplayReturnsTheCurrentCopyAndIgnoresItsBody(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Retry", "")

	var made commentWire
	decode(t, f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "v1"}, "Idempotency-Key", "k"), &made)
	call(t, f.mux, http.MethodPut, V1+"/comments/"+made.ID, map[string]any{"body": "v2"}, f.willC)

	var got commentWire
	rec := f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "something else"}, "Idempotency-Key", "k")
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ID != made.ID || got.Body != "v2" {
		t.Errorf("replay = %d %+v, want the original at v2", rec.Code, got)
	}

	// Deleting frees the key: nothing is left to return.
	call(t, f.mux, http.MethodDelete, V1+"/comments/"+made.ID, nil, f.willC)
	rec = f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "again"}, "Idempotency-Key", "k")
	decode(t, rec, &got)
	if got.ID == made.ID || got.Body != "again" || rec.Header().Get("Idempotent-Replayed") != "false" {
		t.Errorf("after the delete = %+v (replayed %q), want a new comment", got, rec.Header().Get("Idempotent-Replayed"))
	}
}

func TestACommentWithoutAKeyHasNoReplayHeaderAndAKeyIsPerAccount(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Keys", "")

	rec := f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "t"})
	if _, present := rec.Header()["Idempotent-Replayed"]; present {
		t.Error("Idempotent-Replayed sent on a create with no key")
	}

	var a, b commentWire
	decode(t, f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "will's"}, "Idempotency-Key", "shared"), &a)
	rec = f.postComment(t, f.sarahC, map[string]any{"ticket_id": tk.ID, "body": "sarah's"}, "Idempotency-Key", "shared")
	decode(t, rec, &b)
	if a.ID == b.ID || b.Body != "sarah's" || rec.Header().Get("Idempotent-Replayed") != "false" {
		t.Errorf("second account got %+v, want its own comment", b)
	}

	rec = f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "x"}, "Idempotency-Key", "bad\x01key")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a non-printable key: %d, want 422", rec.Code)
	}
}

func TestSixteenConcurrentCommentCreatesWithOneKeyMakeOneComment(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Race", "")

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.postComment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "r"}, "Idempotency-Key", "race")
			var c commentWire
			decode(t, rec, &c)
			mu.Lock()
			ids[c.ID] = true
			mu.Unlock()
			if rec.Code != http.StatusOK {
				t.Errorf("status %d - %.200s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("%d distinct ids, want 1", len(ids))
	}
	if n := len(f.comments(t, f.willC, byTicket(tk.ID))); n != 1 {
		t.Errorf("%d comments, want 1", n)
	}
}

// Comments take the ticket scopes (documents already do): ticket:read lists, ticket:write
// writes, and a token holding only the first cannot comment.
func TestCommentsFollowTheTicketScopes(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Scoped", "")
	c := f.comment(t, f.willC, map[string]any{"ticket_id": tk.ID, "body": "mine"})

	read := createToken(t, f.mux, f.willC, "ticket:read")
	write := createToken(t, f.mux, f.willC, "ticket:write")

	rec := call(t, f.mux, http.MethodPost, V1+"/comments/list", map[string]any{"filter": byTicket(tk.ID)}, "", bearer(read.Token)...)
	if rec.Code != http.StatusOK {
		t.Errorf("ticket:read token lists comments: %d", rec.Code)
	}
	for _, w := range []struct{ method, path string }{
		{http.MethodPost, V1 + "/comments"},
		{http.MethodPut, V1 + "/comments/" + c.ID},
		{http.MethodDelete, V1 + "/comments/" + c.ID},
	} {
		assertCode(t, call(t, f.mux, w.method, w.path, map[string]any{"ticket_id": tk.ID, "body": "x"}, "", bearer(read.Token)...),
			http.StatusForbidden, CodeInsufficientScope)
	}
	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/comments/list", map[string]any{}, "", bearer(write.Token)...),
		http.StatusForbidden, CodeInsufficientScope)

	rec = call(t, f.mux, http.MethodPost, V1+"/comments", map[string]any{"ticket_id": tk.ID, "body": "from a script"}, "", bearer(write.Token)...)
	if rec.Code != http.StatusOK {
		t.Errorf("ticket:write token comments: %d %s", rec.Code, rec.Body)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// LAM-113. Review requests over the wire: create (anchor, reviewers, one transaction),
// reviewer decisions, listing by document and by reviewer, the 403 and 422s, the change feed,
// search, tokens, and what the offsets count.

type reviewWire struct {
	ID           string  `json:"id"`
	DocumentID   string  `json:"document_id"`
	FieldID      *string `json:"field_id"`
	RegionStart  int     `json:"region_start"`
	RegionEnd    int     `json:"region_end"`
	AnchorText   *string `json:"anchor_text"`
	Body         string  `json:"body"`
	ReviewStatus string  `json:"review_status"`
	AuthorID     *string `json:"author_id"`
	Reviewers    []struct {
		AccountID string `json:"account_id"`
	} `json:"reviewers"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r reviewWire) reviewerIDs() []string {
	var out []string
	for _, v := range r.Reviewers {
		out = append(out, v.AccountID)
	}

	return out
}

// reviewDoc is an aspect document whose first field holds text.
func (f feed) reviewDoc(t *testing.T, text string) (doc documentWire, fieldIDs []string) {
	t.Helper()

	typeID, fields := f.aspectType(t, f.willC, "Summary", "Other")
	doc = f.document(t, f.willC, map[string]any{
		"title": "Spec", "type": "aspect", "aspect_type_id": typeID, "project_id": f.project.ID,
		"body": map[string]string{fields[0]: text},
	})

	return doc, fields
}

func (f feed) postReview(t *testing.T, cookie string, body map[string]any, header ...string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/review-requests", body, cookie, header...)
}

func (f feed) review(t *testing.T, cookie string, body map[string]any) reviewWire {
	t.Helper()

	rec := f.postReview(t, cookie, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create review request %v: %d %.300s", body, rec.Code, rec.Body)
	}
	var r reviewWire
	decode(t, rec, &r)

	return r
}

func (f feed) reviews(t *testing.T, cookie string, req map[string]any) []reviewWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/review-requests/list", req, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list review requests: %d %.300s", rec.Code, rec.Body)
	}
	var out struct {
		Items []reviewWire `json:"items"`
	}
	decode(t, rec, &out)

	return out.Items
}

func (f feed) decide(t *testing.T, cookie, id, status string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPut, V1+"/review-requests/"+id, map[string]any{"review_status": status}, cookie)
}

func cond(field string, value any) map[string]any {
	return map[string]any{"field": field, "op": "eq", "value": value}
}

func where(conds ...map[string]any) map[string]any {
	return map[string]any{"filter": map[string]any{"op": "and", "conditions": conds}}
}

func TestCreatingAReviewRequestAnchorsItAndAttachesReviewers(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "Columns assume **one status each**.")

	r := f.review(t, f.willC, map[string]any{
		"document_id": doc.ID, "field_id": fields[0], "region_start": 15, "region_end": 34,
		"body": "Is this still true?", "reviewer_ids": []string{f.sarah.AccountID, f.will.AccountID},
	})
	if r.ID == "" || r.DocumentID != doc.ID || r.FieldID == nil || *r.FieldID != fields[0] ||
		r.RegionStart != 15 || r.RegionEnd != 34 || r.Body != "Is this still true?" ||
		r.ReviewStatus != "pending" || r.AuthorID == nil || *r.AuthorID != f.will.AccountID {
		t.Fatalf("created = %+v", r)
	}
	if r.AnchorText == nil || *r.AnchorText != "**one status each**" {
		t.Errorf("anchor_text = %v, want the quoted range", r.AnchorText)
	}
	if got := r.reviewerIDs(); len(got) != 2 {
		t.Errorf("reviewers = %v", got)
	}

	// A review request is a comment too: the document's thread has it, with its status.
	rec := call(t, f.mux, http.MethodPost, V1+"/comments/list", map[string]any{"filter": byDocument(doc.ID)}, f.sarahC)
	var out struct {
		Items []commentWire `json:"items"`
	}
	decode(t, rec, &out)
	if len(out.Items) != 1 || out.Items[0].ID != r.ID || out.Items[0].ReviewStatus == nil || *out.Items[0].ReviewStatus != "pending" {
		t.Errorf("thread = %+v", out.Items)
	}

	// And it is searchable like one.
	if hits := f.search(t, f.sarahC, map[string]any{"q": "still true"}); len(hits) != 1 {
		t.Errorf("search hits = %+v", hits)
	}
}

func TestOffsetsCountCharactersOfTheStoredMarkdown(t *testing.T) {
	f := newFeed(t)
	// The emoji is one character (code point) and four bytes: offsets are not bytes.
	doc, fields := f.reviewDoc(t, "a😀b é")
	mk := func(start, end int) *httptest.ResponseRecorder {
		return f.postReview(t, f.willC, map[string]any{
			"document_id": doc.ID, "field_id": fields[0], "region_start": start, "region_end": end,
			"body": "x", "reviewer_ids": []string{f.sarah.AccountID},
		})
	}

	rec := mk(1, 4)
	if rec.Code != http.StatusOK {
		t.Fatalf("range inside: %d %s", rec.Code, rec.Body)
	}
	var r reviewWire
	decode(t, rec, &r)
	if r.AnchorText == nil || *r.AnchorText != "😀b " {
		t.Errorf("anchor_text = %v, want %q", r.AnchorText, "😀b ")
	}

	if rec := mk(5, 5); rec.Code != http.StatusOK { // the caret at the very end
		t.Errorf("caret at end: %d %s", rec.Code, rec.Body)
	}
	wantDocProblem(t, mk(0, 6), http.StatusUnprocessableEntity, "unprocessable_entity", "body.region_end")
}

func TestAFieldWithNoTextTakesOnlyACaretAtZero(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "text")
	body := func(end int) map[string]any {
		return map[string]any{"document_id": doc.ID, "field_id": fields[1], "region_start": 0, "region_end": end,
			"body": "x", "reviewer_ids": []string{f.sarah.AccountID}}
	}

	r := f.review(t, f.willC, body(0))
	if r.AnchorText == nil || *r.AnchorText != "" {
		t.Errorf("anchor_text = %v, want the empty string", r.AnchorText)
	}
	wantDocProblem(t, f.postReview(t, f.willC, body(1)), http.StatusUnprocessableEntity, "unprocessable_entity", "body.region_end")
}

func TestAReviewRequestIsRefusedWithTheFieldNamed(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	_, other := f.aspectType(t, f.willC, "Elsewhere")
	plain := f.document(t, f.willC, map[string]any{"title": "P", "project_id": f.project.ID})
	sarah := []string{f.sarah.AccountID}

	base := func(over map[string]any) map[string]any {
		m := map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
			"body": "look", "reviewer_ids": sarah}
		for k, v := range over {
			m[k] = v
		}

		return m
	}

	for _, tc := range []struct {
		name     string
		body     map[string]any
		location string
	}{
		{"field of another type", base(map[string]any{"field_id": other[0]}), "body.field_id"},
		{"field on a normal document", base(map[string]any{"document_id": plain.ID}), "body.field_id"},
		{"no field on an aspect document", map[string]any{"document_id": doc.ID, "region_start": 0, "region_end": 1, "body": "x", "reviewer_ids": sarah}, "body.field_id"},
		{"non-member reviewer", base(map[string]any{"reviewer_ids": []string{f.sarah.AccountID, f.carol.AccountID}}), "body.reviewer_ids[1]"},
		{"unknown account", base(map[string]any{"reviewer_ids": []string{"99999999-9999-4999-8999-999999999999"}}), "body.reviewer_ids[0]"},
		{"reviewer twice", base(map[string]any{"reviewer_ids": []string{f.sarah.AccountID, f.sarah.AccountID}}), "body.reviewer_ids[1]"},
		{"no reviewers", base(map[string]any{"reviewer_ids": []string{}}), "body.reviewer_ids"},
		{"backwards", base(map[string]any{"region_start": 5, "region_end": 4}), "body.region_end"},
		{"negative", base(map[string]any{"region_start": -1}), "body.region_start"},
		{"past the end", base(map[string]any{"region_end": 12}), "body.region_end"},
		{"blank note", base(map[string]any{"body": "  "}), "body.body"},
		{"no document", map[string]any{"field_id": fields[0], "region_start": 0, "region_end": 1, "body": "x", "reviewer_ids": sarah}, ""},
	} {
		rec := f.postReview(t, f.willC, tc.body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %.200s, want 422", tc.name, rec.Code, rec.Body)
			continue
		}
		if tc.location != "" {
			wantDocProblem(t, rec, http.StatusUnprocessableEntity, "unprocessable_entity", tc.location)
		}
	}

	// Nothing was left behind by any refusal (the reviewers go in the same transaction).
	if got := f.reviews(t, f.willC, where(cond("document_id", doc.ID))); len(got) != 0 {
		t.Errorf("refused creates left %+v", got)
	}
	var n int
	if err := f.pool.QueryRow(testCtx(t), `SELECT count(*) FROM comment_reviewer`).Scan(&n); err != nil || n != 0 {
		t.Errorf("comment_reviewer rows = %d, %v", n, err)
	}

	// A document outside the workspace is not found.
	rec := f.postReview(t, f.carolC, base(nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("other workspace: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestReviewersApproveAndRequestChanges(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})

	rec := f.decide(t, f.sarahC, r.ID, "approved")
	var got reviewWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ReviewStatus != "approved" || !got.UpdatedAt.After(r.UpdatedAt) || got.Body != "ok?" ||
		len(got.Reviewers) != 1 || got.AnchorText == nil {
		t.Fatalf("approve = %d %+v", rec.Code, got)
	}

	rec = f.decide(t, f.sarahC, r.ID, "changes_requested")
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ReviewStatus != "changes_requested" {
		t.Fatalf("request changes = %d %+v", rec.Code, got)
	}

	rec = f.decide(t, f.sarahC, r.ID, "pending")
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ReviewStatus != "pending" {
		t.Fatalf("reopen = %d %+v", rec.Code, got)
	}

	// Null and unknown values are 422.
	for _, v := range []any{nil, "rejected", ""} {
		rec = call(t, f.mux, http.MethodPut, V1+"/review-requests/"+r.ID, map[string]any{"review_status": v}, f.sarahC)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("review_status %v: %d, want 422", v, rec.Code)
		}
	}
}

func TestOnlyAListedReviewerMayDecide(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})

	// The author can see it and is not a reviewer.
	wantDocProblem(t, f.decide(t, f.willC, r.ID, "approved"), http.StatusForbidden, "not_a_reviewer", "")
	// Another workspace cannot see it at all.
	wantDocProblem(t, f.decide(t, f.carolC, r.ID, "approved"), http.StatusNotFound, "review_request_not_found", "")
	// Absent.
	wantDocProblem(t, f.decide(t, f.sarahC, "99999999-9999-4999-8999-999999999999", "approved"), http.StatusNotFound, "review_request_not_found", "")
	// A plain comment is not a review request.
	plain := f.comment(t, f.sarahC, map[string]any{"document_id": doc.ID, "body": "just a remark"})
	wantDocProblem(t, f.decide(t, f.sarahC, plain.ID, "approved"), http.StatusNotFound, "review_request_not_found", "")

	if got := f.reviews(t, f.willC, where(cond("document_id", doc.ID))); len(got) != 1 || got[0].ReviewStatus != "pending" {
		t.Errorf("after refusals = %+v", got)
	}
}

func TestListingByDocumentByReviewerAndByStatus(t *testing.T) {
	f := newFeed(t)
	docA, fa := f.reviewDoc(t, "alpha beta")
	docB, fb := f.reviewDoc(t, "gamma delta")
	mk := func(doc documentWire, field string, reviewers ...string) reviewWire {
		return f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": field, "region_start": 0, "region_end": 5,
			"body": "r", "reviewer_ids": reviewers})
	}
	a1 := mk(docA, fa[0], f.sarah.AccountID)
	a2 := mk(docA, fa[0], f.will.AccountID)
	b1 := mk(docB, fb[0], f.sarah.AccountID, f.will.AccountID)
	f.decide(t, f.willC, a2.ID, "approved")

	ids := func(rs []reviewWire) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.ID] = true
		}

		return m
	}
	want := func(name string, got []reviewWire, wantIDs ...string) {
		t.Helper()
		m := ids(got)
		if len(got) != len(wantIDs) {
			t.Errorf("%s = %d requests, want %d", name, len(got), len(wantIDs))
		}
		for _, id := range wantIDs {
			if !m[id] {
				t.Errorf("%s is missing %s", name, id)
			}
		}
	}

	// Open is the default: a2 is approved and left out.
	want("everything open", f.reviews(t, f.sarahC, map[string]any{}), a1.ID, b1.ID)
	want("by document", f.reviews(t, f.sarahC, where(cond("document_id", docA.ID))), a1.ID)
	want("assigned to me (sarah)", f.reviews(t, f.sarahC, where(cond("reviewer_id", f.sarah.AccountID))), a1.ID, b1.ID)
	want("assigned to will", f.reviews(t, f.sarahC, where(cond("reviewer_id", f.will.AccountID))), b1.ID)
	want("document and reviewer", f.reviews(t, f.sarahC, where(cond("document_id", docB.ID), cond("reviewer_id", f.will.AccountID))), b1.ID)
	want("approved", f.reviews(t, f.sarahC, where(cond("review_status", "approved"))), a2.ID)
	want("pending explicitly", f.reviews(t, f.sarahC, where(cond("review_status", "pending"))), a1.ID, b1.ID)
	want("approved, assigned to will", f.reviews(t, f.sarahC, where(cond("review_status", "approved"), cond("reviewer_id", f.will.AccountID))), a2.ID)

	// Plain comments never appear, and another workspace sees nothing.
	f.comment(t, f.willC, map[string]any{"document_id": docA.ID, "body": "plain"})
	want("plain comments excluded", f.reviews(t, f.sarahC, where(cond("document_id", docA.ID))), a1.ID)
	want("other workspace", f.reviews(t, f.carolC, map[string]any{}))

	// Paging.
	rec := call(t, f.mux, http.MethodPost, V1+"/review-requests/list", map[string]any{"limit": 1}, f.sarahC)
	var page struct {
		Items      []reviewWire `json:"items"`
		NextCursor *string      `json:"next_cursor"`
		HasMore    bool         `json:"has_more"`
	}
	decode(t, rec, &page)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("first page = %+v", page)
	}
	rec = call(t, f.mux, http.MethodPost, V1+"/review-requests/list", map[string]any{"limit": 1, "cursor": *page.NextCursor}, f.sarahC)
	var next struct {
		Items   []reviewWire `json:"items"`
		HasMore bool         `json:"has_more"`
	}
	decode(t, rec, &next)
	if len(next.Items) != 1 || next.HasMore || next.Items[0].ID == page.Items[0].ID {
		t.Errorf("second page = %+v", next)
	}
}

func TestReviewRequestsAreOnTheChangeFeed(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	watcher := f.open(t, f.cookie(f.sarahC))

	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})
	ev := readEvent(t, watcher)
	if ev.Type != "review_request.created" || ev.CommentID != r.ID || ev.DocumentID != doc.ID || ev.TicketID != "" ||
		ev.ActorID != f.will.AccountID || ev.ReviewRequest["id"] != r.ID || ev.ReviewRequest["review_status"] != "pending" ||
		ev.Comment != nil || ev.Version != 0 {
		t.Errorf("created event = %+v", ev)
	}

	// Refused and unchanged writes publish nothing.
	f.decide(t, f.willC, r.ID, "approved")
	f.decide(t, f.sarahC, r.ID, "pending")
	f.postReview(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 99,
		"body": "x", "reviewer_ids": []string{f.sarah.AccountID}})

	f.decide(t, f.sarahC, r.ID, "changes_requested")
	ev = readEvent(t, watcher)
	if ev.Type != "review_request.updated" || ev.CommentID != r.ID || ev.ActorID != f.sarah.AccountID ||
		ev.ReviewRequest["review_status"] != "changes_requested" {
		t.Errorf("updated event = %+v, want the first one after the create", ev)
	}

	// Deleting it through the comment endpoint is a comment.deleted that carries the status.
	call(t, f.mux, http.MethodDelete, V1+"/comments/"+r.ID, nil, f.willC)
	if ev = readEvent(t, watcher); ev.Type != "comment.deleted" || ev.CommentID != r.ID || ev.Comment["review_status"] != "changes_requested" {
		t.Errorf("deleted event = %+v", ev)
	}
	var n int
	if err := f.pool.QueryRow(testCtx(t), `SELECT count(*) FROM comment_reviewer WHERE comment_id = $1::uuid`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("reviewer rows after delete = %d, %v", n, err)
	}
}

func TestReviewRequestsFollowTheTicketScopes(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	body := map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "from an agent", "reviewer_ids": []string{f.sarah.AccountID}}

	read := createToken(t, f.mux, f.willC, "ticket:read")
	write := createToken(t, f.mux, f.willC, "ticket:write")

	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/review-requests", body, "", bearer(read.Token)...), http.StatusForbidden, CodeInsufficientScope)
	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/review-requests/list", map[string]any{}, "", bearer(write.Token)...), http.StatusForbidden, CodeInsufficientScope)

	rec := call(t, f.mux, http.MethodPost, V1+"/review-requests", body, "", bearer(write.Token)...)
	if rec.Code != http.StatusOK {
		t.Fatalf("token creates a review request: %d %s", rec.Code, rec.Body)
	}
	var r reviewWire
	decode(t, rec, &r)
	if r.AuthorID == nil || *r.AuthorID != f.will.AccountID {
		t.Errorf("token author = %v, want the token owner", r.AuthorID)
	}
	assertCode(t, call(t, f.mux, http.MethodPut, V1+"/review-requests/"+r.ID, map[string]any{"review_status": "approved"}, "", bearer(read.Token)...),
		http.StatusForbidden, CodeInsufficientScope)

	if got := f.reviews(t, f.willC, where(cond("document_id", doc.ID))); len(got) != 1 || !strings.Contains(got[0].Body, "agent") {
		t.Errorf("listed = %+v", got)
	}
}

func TestANormalDocumentIsReviewedInItsContent(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Notes", "project_id": f.project.ID,
		"body": map[string]string{"content": "# Meeting\nShip it on Friday."}})
	body := func(start, end int) map[string]any {
		return map[string]any{"document_id": doc.ID, "region_start": start, "region_end": end,
			"body": "Friday?", "reviewer_ids": []string{f.sarah.AccountID}}
	}

	r := f.review(t, f.willC, body(21, 27))
	if r.FieldID != nil || r.DocumentID != doc.ID || r.ReviewStatus != "pending" || r.AnchorText == nil || *r.AnchorText != "Friday" {
		t.Fatalf("created = %+v (anchor %v)", r, r.AnchorText)
	}
	wantDocProblem(t, f.postReview(t, f.willC, body(0, 99)), http.StatusUnprocessableEntity, "unprocessable_entity", "body.region_end")

	if rec := f.decide(t, f.sarahC, r.ID, "approved"); rec.Code != http.StatusOK {
		t.Errorf("approve: %d %s", rec.Code, rec.Body)
	}
	if got := f.reviews(t, f.sarahC, where(cond("document_id", doc.ID), cond("review_status", "approved"))); len(got) != 1 {
		t.Errorf("listed = %+v", got)
	}

	// An empty normal document takes only the caret at 0.
	empty := f.document(t, f.willC, map[string]any{"title": "Empty", "project_id": f.project.ID})
	rec := f.postReview(t, f.willC, map[string]any{"document_id": empty.ID, "region_start": 0, "region_end": 0,
		"body": "x", "reviewer_ids": []string{f.sarah.AccountID}})
	if rec.Code != http.StatusOK {
		t.Errorf("caret in an empty document: %d %s", rec.Code, rec.Body)
	}
}

func TestTheSameKeyTwiceIsOneReviewRequestAndOneEvent(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	watcher := f.open(t, f.cookie(f.sarahC))
	body := map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}}

	first := f.postReview(t, f.willC, body, "Idempotency-Key", "rk-1")
	if first.Code != http.StatusOK || first.Header().Get("Idempotent-Replayed") != "false" {
		t.Fatalf("first: %d replayed=%q %s", first.Code, first.Header().Get("Idempotent-Replayed"), first.Body)
	}
	var a reviewWire
	decode(t, first, &a)
	if ev := readEvent(t, watcher); ev.Type != "review_request.created" {
		t.Fatalf("event = %+v", ev)
	}

	f.decide(t, f.sarahC, a.ID, "approved")
	readEvent(t, watcher) // review_request.updated

	// The replay ignores its body, returns the current copy, and publishes nothing.
	body["body"] = "different"
	second := f.postReview(t, f.willC, body, "Idempotency-Key", "rk-1")
	var b reviewWire
	decode(t, second, &b)
	if second.Code != http.StatusOK || second.Header().Get("Idempotent-Replayed") != "true" ||
		b.ID != a.ID || b.Body != "ok?" || b.ReviewStatus != "approved" || len(b.Reviewers) != 1 {
		t.Fatalf("replay: %d %q %+v", second.Code, second.Header().Get("Idempotent-Replayed"), b)
	}
	if got := f.reviews(t, f.willC, where(cond("document_id", doc.ID), cond("review_status", "approved"))); len(got) != 1 {
		t.Errorf("review requests = %+v, want one", got)
	}
	f.decide(t, f.sarahC, a.ID, "pending")
	if ev := readEvent(t, watcher); ev.Type != "review_request.updated" {
		t.Errorf("next event = %+v, want the update (the replay published nothing)", ev)
	}

	// Keys are per account, and a key of a plain comment is not answered with that comment.
	other := f.postReview(t, f.sarahC, body, "Idempotency-Key", "rk-1")
	if other.Header().Get("Idempotent-Replayed") != "false" {
		t.Errorf("another account's first use replayed: %d %s", other.Code, other.Body)
	}
	f.postComment(t, f.willC, map[string]any{"document_id": doc.ID, "body": "plain"}, "Idempotency-Key", "ck-1")
	wantDocProblem(t, f.postReview(t, f.willC, body, "Idempotency-Key", "ck-1"),
		http.StatusUnprocessableEntity, "unprocessable_entity", "header.Idempotency-Key")
}

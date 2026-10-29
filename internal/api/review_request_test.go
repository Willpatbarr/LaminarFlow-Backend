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

	EditedAt     *time.Time `json:"edited_at"`
	DecidedBy    *string    `json:"decided_by"`
	DecidedAt    *time.Time `json:"decided_at"`
	DecisionNote *string    `json:"decision_note"`
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

// LAM-122. An approval goes stale when the reviewed text materially changes.

func (f feed) putDocText(t *testing.T, doc documentWire, field, text string) {
	t.Helper()

	rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+doc.ID, map[string]any{
		"title": "Spec", "type": doc.Type, "aspect_type_id": doc.AspectTypeID, "body": map[string]string{field: text},
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit document: %d %.300s", rec.Code, rec.Body)
	}
}

func (f feed) oneReview(t *testing.T, id string) staleWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/review-requests/list", where(cond("document_id", f.docOf(t, id)),
		map[string]any{"field": "review_status", "op": "in", "value": []string{"pending", "approved", "changes_requested"}}), f.willC)
	var out struct {
		Items []staleWire `json:"items"`
	}
	decode(t, rec, &out)
	for _, r := range out.Items {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("review request %s not listed: %s", id, rec.Body)

	return staleWire{}
}

func (f feed) docOf(t *testing.T, id string) string {
	t.Helper()

	var doc string
	if err := f.pool.QueryRow(testCtx(t), `SELECT document_id::text FROM comment WHERE id = $1::uuid`, id).Scan(&doc); err != nil {
		t.Fatal(err)
	}

	return doc
}

type staleWire struct {
	ID           string  `json:"id"`
	ReviewStatus string  `json:"review_status"`
	StaleReason  *string `json:"stale_reason"`
}

func TestATypoKeepsTheApprovalAndARewriteSendsItBackToPending(t *testing.T) {
	f := newFeed(t)
	const text = "Columns assume one status each, and a board shows them."
	doc, fields := f.reviewDoc(t, text)
	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 31,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})
	if rec := f.decide(t, f.sarahC, r.ID, "approved"); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	watcher := f.open(t, f.cookie(f.sarahC))

	// One character of 31 is under 10%: the approval stands, and nothing is announced about it.
	f.putDocText(t, doc, fields[0], "Columns assume one status eac, and a board shows them.")
	if got := f.oneReview(t, r.ID); got.ReviewStatus != "approved" || got.StaleReason != nil {
		t.Fatalf("after a typo = %+v", got)
	}

	// Rewriting the sentence changes far more than 10% of it.
	f.putDocText(t, doc, fields[0], "Every column is fed by a label now, and a board shows them.")
	got := f.oneReview(t, r.ID)
	if got.ReviewStatus != "pending" || got.StaleReason == nil || *got.StaleReason != "anchored text changed" {
		t.Fatalf("after a rewrite = %+v", got)
	}

	// It is announced like any status change, with the reason.
	var ev eventWire
	for {
		if ev = readEvent(t, watcher); ev.Type == "review_request.updated" {
			break
		}
	}
	if ev.CommentID != r.ID || ev.DocumentID != doc.ID || ev.ReviewRequest["review_status"] != "pending" ||
		ev.ReviewRequest["stale_reason"] != "anchored text changed" {
		t.Errorf("event = %+v", ev)
	}

	// Approving again clears the reason; so does asking for changes.
	if rec := f.decide(t, f.sarahC, r.ID, "approved"); rec.Code != http.StatusOK {
		t.Fatalf("re-approve: %d %s", rec.Code, rec.Body)
	}
	if got := f.oneReview(t, r.ID); got.ReviewStatus != "approved" || got.StaleReason != nil {
		t.Errorf("after approving again = %+v", got)
	}
}

func TestOnlyApprovedRequestsGoStaleAndOnlyForTheirOwnField(t *testing.T) {
	f := newFeed(t)
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Other")
	doc := f.document(t, f.willC, map[string]any{"title": "Spec", "type": "aspect", "aspect_type_id": typeID,
		"project_id": f.project.ID, "body": map[string]string{fields[0]: "alpha beta gamma", fields[1]: "one two three"}})
	mk := func(field string) reviewWire {
		return f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": field, "region_start": 0, "region_end": 13,
			"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})
	}
	approved, pending, other := mk(fields[0]), mk(fields[0]), mk(fields[1])
	f.decide(t, f.sarahC, approved.ID, "approved")
	f.decide(t, f.sarahC, other.ID, "approved")

	rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+doc.ID, map[string]any{
		"title": "Spec", "type": "aspect", "aspect_type_id": typeID,
		"body": map[string]string{fields[0]: "completely different words", fields[1]: "one two three"},
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}

	if got := f.oneReview(t, approved.ID); got.ReviewStatus != "pending" || got.StaleReason == nil {
		t.Errorf("approved, text rewritten = %+v", got)
	}
	if got := f.oneReview(t, pending.ID); got.ReviewStatus != "pending" || got.StaleReason != nil {
		t.Errorf("already pending = %+v, want no reason", got)
	}
	if got := f.oneReview(t, other.ID); got.ReviewStatus != "approved" || got.StaleReason != nil {
		t.Errorf("approved, other field untouched = %+v", got)
	}
}

func TestTheStaleThresholdIsASetting(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "0123456789")
	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 10,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})

	var ws string
	if err := f.pool.QueryRow(testCtx(t), `SELECT workspace_id::text FROM workspace_member WHERE account_id = $1::uuid`, f.will.AccountID).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	set := func(value any) *httptest.ResponseRecorder {
		return call(t, f.mux, http.MethodPut, V1+"/settings/review.stale_threshold_percent?scope=workspace&target_id="+ws,
			map[string]any{"value": value}, f.willC)
	}

	// At the default of 10%, 2 of 10 characters would be stale; at 50% it is not.
	if rec := set(50); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	f.decide(t, f.sarahC, r.ID, "approved")
	f.putDocText(t, doc, fields[0], "01234567xx")
	if got := f.oneReview(t, r.ID); got.ReviewStatus != "approved" {
		t.Errorf("2 of 10 at 50%% = %+v", got)
	}
	if rec := set(10); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	f.putDocText(t, doc, fields[0], "01234567xy")
	if got := f.oneReview(t, r.ID); got.ReviewStatus != "approved" {
		t.Errorf("1 of 10 at 10%% is not more than 10%%: %+v", got)
	}
	f.putDocText(t, doc, fields[0], "0123456zzz")
	if got := f.oneReview(t, r.ID); got.ReviewStatus != "pending" {
		t.Errorf("3 of 10 at 10%% = %+v", got)
	}

	// A bad value or an unregistered key is a 422.
	if rec := set(101); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("101: %d", rec.Code)
	}
}

// LAM-132: a decision records who, when and the optional note; a status change is not an edit
// of the note; a note edit is; reopening keeps the last decision; a retry is harmless.
func TestADecisionRecordsWhoAndNoteAndIsNotAnEdit(t *testing.T) {
	f := newFeed(t)
	doc, fields := f.reviewDoc(t, "hello world")
	r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "field_id": fields[0], "region_start": 0, "region_end": 5,
		"body": "ok?", "reviewer_ids": []string{f.sarah.AccountID}})
	if r.EditedAt != nil || r.DecidedBy != nil || r.DecidedAt != nil || r.DecisionNote != nil {
		t.Fatalf("new request already decided or edited: %+v", r)
	}

	put := func(body map[string]any) (*httptest.ResponseRecorder, reviewWire) {
		rec := call(t, f.mux, http.MethodPut, V1+"/review-requests/"+r.ID, body, f.sarahC)
		var got reviewWire
		decode(t, rec, &got)

		return rec, got
	}

	rec, got := put(map[string]any{"review_status": "changes_requested", "decision_note": "  please shorten  "})
	if rec.Code != http.StatusOK || got.DecidedBy == nil || *got.DecidedBy != f.sarah.AccountID || got.DecidedAt == nil ||
		got.DecisionNote == nil || *got.DecisionNote != "please shorten" {
		t.Fatalf("request changes = %d %+v", rec.Code, got)
	}
	// B8: updated_at moved (the event and sync read it), edited_at did not.
	if !got.UpdatedAt.After(r.UpdatedAt) || got.EditedAt != nil {
		t.Errorf("status change: updated_at %v (was %v), edited_at %v", got.UpdatedAt, r.UpdatedAt, got.EditedAt)
	}

	// A retry of the same call changes nothing, whatever note it carries.
	if rec, again := put(map[string]any{"review_status": "changes_requested", "decision_note": "other"}); rec.Code != http.StatusOK ||
		!again.UpdatedAt.Equal(got.UpdatedAt) || again.DecisionNote == nil || *again.DecisionNote != "please shorten" {
		t.Errorf("retry = %d %+v", rec.Code, again)
	}

	// Approving without a note clears the old note; blank is none.
	if _, ok := put(map[string]any{"review_status": "approved", "decision_note": "   "}); ok.DecisionNote != nil || ok.DecidedBy == nil {
		t.Errorf("approve without note = %+v", ok)
	}

	// Reopening keeps the last decision, and a note with it is refused.
	if _, pend := put(map[string]any{"review_status": "pending"}); pend.ReviewStatus != "pending" || pend.DecidedBy == nil || pend.DecidedAt == nil {
		t.Errorf("reopen lost the last decision: %+v", pend)
	}
	rec, _ = put(map[string]any{"review_status": "pending", "decision_note": "x"})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "body.decision_note") {
		t.Errorf("note with pending: %d %s, want 422 at body.decision_note", rec.Code, rec.Body)
	}

	// Editing the note is an edit.
	rec = call(t, f.mux, http.MethodPut, V1+"/comments/"+r.ID, map[string]any{"body": "ok? (reworded)"}, f.willC)
	var c commentWire
	decode(t, rec, &c)
	if rec.Code != http.StatusOK || c.EditedAt == nil {
		t.Fatalf("edit note = %d %+v", rec.Code, c)
	}
	if got := f.reviews(t, f.willC, where(cond("document_id", doc.ID), cond("review_status", "pending"))); len(got) != 1 ||
		got[0].EditedAt == nil || got[0].DecisionNote != nil && *got[0].DecisionNote != "" {
		t.Errorf("listed after edit = %+v", got)
	}
}

// LAM-136. Turning a document into an aspect carries its review anchors to the field the text
// moved to.

func (f feed) makeAspect(t *testing.T, doc documentWire, typeID string, body map[string]string) {
	t.Helper()

	rec := call(t, f.mux, http.MethodPut, V1+"/documents/"+doc.ID, map[string]any{
		"title": "Notes", "type": "aspect", "aspect_type_id": typeID, "body": body,
	}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("convert document: %d %.300s", rec.Code, rec.Body)
	}
}

func TestConvertingToAnAspectMovesReviewAnchorsToTheFirstField(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Notes", "project_id": f.project.ID,
		"body": map[string]string{"content": "# Meeting\nShip it on Friday."}})
	mk := func(status string) reviewWire {
		r := f.review(t, f.willC, map[string]any{"document_id": doc.ID, "region_start": 21, "region_end": 27,
			"body": "Friday?", "reviewer_ids": []string{f.sarah.AccountID}})
		if status != "pending" {
			if rec := f.decide(t, f.sarahC, r.ID, status); rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", status, rec.Code, rec.Body)
			}
		}

		return r
	}
	pending, approved, changes := mk("pending"), mk("approved"), mk("changes_requested")
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Other")
	watcher := f.open(t, f.cookie(f.sarahC))

	f.makeAspect(t, doc, typeID, map[string]string{})

	seen := map[string]bool{}
	for len(seen) < 3 {
		ev := readEvent(t, watcher)
		if ev.Type == "review_request.updated" && ev.ReviewRequest["field_id"] == fields[0] {
			seen[ev.CommentID] = true
		}
	}

	want := map[string]string{pending.ID: "pending", approved.ID: "approved", changes.ID: "changes_requested"}
	for _, r := range f.reviews(t, f.willC, where(cond("document_id", doc.ID))) {
		if r.FieldID == nil || *r.FieldID != fields[0] || r.RegionStart != 21 || r.RegionEnd != 27 ||
			r.ReviewStatus != want[r.ID] {
			t.Errorf("after conversion %+v", r)
		}
		if got := f.oneReview(t, r.ID); got.StaleReason != nil {
			t.Errorf("%s went stale: %v", r.ID, *got.StaleReason)
		}
	}
}

func TestConvertingWithAnEditedFirstFieldFindsTheAnchorTextOrLeavesItAlone(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Notes", "project_id": f.project.ID,
		"body": map[string]string{"content": "# Meeting\nShip it on Friday."}})
	review := func(start, end int) reviewWire {
		return f.review(t, f.willC, map[string]any{"document_id": doc.ID, "region_start": start, "region_end": end,
			"body": "?", "reviewer_ids": []string{f.sarah.AccountID}})
	}
	friday, meeting := review(21, 27), review(2, 9)
	if rec := f.decide(t, f.sarahC, friday.ID, "approved"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Other")

	// The request fills the first field itself, so the old text is dropped: "Friday" is still
	// there, shifted; "Meeting" is gone.
	f.makeAspect(t, doc, typeID, map[string]string{fields[0]: "Plan\nShip it on Friday."})

	for _, r := range f.reviews(t, f.willC, where(cond("document_id", doc.ID))) {
		switch r.ID {
		case friday.ID:
			if r.FieldID == nil || *r.FieldID != fields[0] || r.RegionStart != 16 || r.RegionEnd != 22 ||
				r.ReviewStatus != "approved" {
				t.Errorf("found anchor = %+v", r)
			}
		case meeting.ID:
			if r.FieldID != nil || r.RegionStart != 2 || r.RegionEnd != 9 {
				t.Errorf("anchor text not found should be untouched, got %+v", r)
			}
		}
	}
}

func TestConvertingToAnAspectAlsoMovesAPlainAnchoredComment(t *testing.T) {
	f := newFeed(t)
	doc := f.document(t, f.willC, map[string]any{"title": "Notes", "project_id": f.project.ID,
		"body": map[string]string{"content": "# Meeting\nShip it on Friday."}})
	rec := call(t, f.mux, http.MethodPost, V1+"/comments", map[string]any{"document_id": doc.ID,
		"region_start": 21, "region_end": 27, "body": "Friday?"}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("comment: %d %s", rec.Code, rec.Body)
	}
	var c struct {
		ID string `json:"id"`
	}
	decode(t, rec, &c)
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Other")
	watcher := f.open(t, f.cookie(f.sarahC))

	f.makeAspect(t, doc, typeID, map[string]string{})

	var ev eventWire
	for {
		if ev = readEvent(t, watcher); ev.Type == "comment.updated" {
			break
		}
	}
	if ev.CommentID != c.ID || ev.Comment["field_id"] != fields[0] {
		t.Errorf("event = %+v", ev)
	}
	var field *string
	var start, end int
	if err := f.pool.QueryRow(testCtx(t), `SELECT field_id::text, region_start, region_end FROM comment WHERE id = $1::uuid`, c.ID).
		Scan(&field, &start, &end); err != nil || field == nil || *field != fields[0] || start != 21 || end != 27 {
		t.Errorf("row = %v %d %d, %v", field, start, end, err)
	}
}

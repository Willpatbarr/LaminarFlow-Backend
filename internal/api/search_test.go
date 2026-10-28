package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
)

// LAM-100. The search endpoint end to end: every source is found through the live write
// paths, ranking and snippets, scope, validation, and a rebuild agreeing with the live
// index.

type searchHitWire struct {
	Type       string  `json:"type"`
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Snippet    string  `json:"snippet"`
	FieldLabel *string `json:"field_label"`
	Score      float64 `json:"score"`
}

func (f feed) search(t *testing.T, cookie string, body map[string]any) []searchHitWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/search/list", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %v: status %d - %.300s", body, rec.Code, rec.Body)
	}
	var out struct {
		Items []searchHitWire `json:"items"`
	}
	decode(t, rec, &out)

	return out.Items
}

func (f feed) ticketWith(t *testing.T, title, description string) ticketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/tickets", map[string]any{
		"project_id": f.project.ID, "title": title, "description": description, "status_id": nil, "assignee_id": nil,
	}, f.willC)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create ticket: status %d, body %s", rec.Code, rec.Body)
	}
	var out ticketWire
	decode(t, rec, &out)

	return out
}

func ids(hits []searchHitWire) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Type + ":" + h.ID
	}

	return out
}

func TestSearchRequiresACaller(t *testing.T) {
	f := newFeed(t)

	if rec := call(t, f.mux, http.MethodPost, V1+"/search/list", map[string]any{"q": "x"}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("search without a caller: status %d, want 401", rec.Code)
	}
}

func TestSearchFindsEverySourceByItsOwnText(t *testing.T) {
	f := newFeed(t)

	tk := f.ticketWith(t, "Reconcile ledger totals", "")
	normal := f.document(t, f.willC, map[string]any{
		"title": "Onboarding", "type": "normal", "body": map[string]any{"content": "Remember to provision the gizmo first"},
	})
	typeID, fields := f.aspectType(t, f.willC, "Summary", "Owner")
	aspect := f.document(t, f.willC, map[string]any{
		"title": "Billing service", "type": "aspect", "aspect_type_id": typeID,
		"body": map[string]any{fields[1]: "Marguerite Okonkwo"},
	})

	for _, tc := range []struct {
		name, q, wantType, wantID string
		wantLabel                 string
	}{
		{"ticket by title", "ledger", "ticket", tk.ID, ""},
		{"document by body text", "gizmo", "document", normal.ID, ""},
		{"document by title", "onboarding", "document", normal.ID, ""},
		{"aspect document by a field value", "marguerite", "document", aspect.ID, "Owner"},
		{"project by name", "core", "project", f.project.ID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := f.search(t, f.willC, map[string]any{"q": tc.q})
			if len(hits) == 0 {
				t.Fatalf("q=%q found nothing", tc.q)
			}
			h := hits[0]
			if h.Type != tc.wantType || h.ID != tc.wantID {
				t.Fatalf("q=%q first hit = %s:%s, want %s:%s (all: %v)", tc.q, h.Type, h.ID, tc.wantType, tc.wantID, ids(hits))
			}
			switch {
			case tc.wantLabel == "" && h.FieldLabel != nil && tc.wantType != "document":
				t.Errorf("field_label = %q, want null for a %s", *h.FieldLabel, tc.wantType)
			case tc.wantLabel != "" && (h.FieldLabel == nil || *h.FieldLabel != tc.wantLabel):
				t.Errorf("field_label = %v, want %q", h.FieldLabel, tc.wantLabel)
			}
		})
	}
}

func TestSearchTypeFilterAndLimit(t *testing.T) {
	f := newFeed(t)

	f.ticketWith(t, "Pelican alpha", "")
	f.ticketWith(t, "Pelican beta", "")
	doc := f.document(t, f.willC, map[string]any{
		"title": "Pelican notes", "type": "normal", "body": map[string]any{"content": "birds"},
	})

	if got := f.search(t, f.willC, map[string]any{"q": "pelican"}); len(got) != 3 {
		t.Fatalf("unfiltered = %v, want 3", ids(got))
	}

	docs := f.search(t, f.willC, map[string]any{"q": "pelican", "type": "document"})
	if len(docs) != 1 || docs[0].ID != doc.ID {
		t.Errorf("type=document = %v, want only %s", ids(docs), doc.ID)
	}

	if got := f.search(t, f.willC, map[string]any{"q": "pelican", "limit": 2}); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
}

func TestSearchRanksATitleMatchAboveABodyMatch(t *testing.T) {
	f := newFeed(t)

	f.ticketWith(t, "Unrelated", "the zebra is mentioned only in the description")
	titled := f.ticketWith(t, "Zebra crossing", "")

	hits := f.search(t, f.willC, map[string]any{"q": "zebra"})
	if len(hits) != 2 {
		t.Fatalf("hits = %v, want 2", ids(hits))
	}
	if hits[0].ID != titled.ID {
		t.Errorf("first = %s, want the title match %s", hits[0].ID, titled.ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("scores %v then %v, want strictly descending", hits[0].Score, hits[1].Score)
	}
}

func TestSearchSnippetsAreEscapedAndMarked(t *testing.T) {
	f := newFeed(t)

	f.ticketWith(t, "Markup", `1 < 2 && 4 > 3 and "the" quokka`)

	hits := f.search(t, f.willC, map[string]any{"q": "quokka"})
	if len(hits) != 1 {
		t.Fatalf("hits = %v", ids(hits))
	}

	s := hits[0].Snippet
	if strings.Contains(s, "< 2") || strings.Contains(s, "> 3") || strings.Contains(s, "&& ") {
		t.Errorf("snippet carries unescaped characters: %q", s)
	}
	if !strings.Contains(s, "&lt; 2") || !strings.Contains(s, "&amp;&amp;") || !strings.Contains(s, "&gt; 3") {
		t.Errorf("snippet = %q, want the content escaped", s)
	}
	if !strings.Contains(s, "<mark>quokka</mark>") {
		t.Errorf("snippet = %q, want <mark>quokka</mark>", s)
	}
}

func TestSearchDoesNotReturnArchivedOrOtherWorkspacesRows(t *testing.T) {
	f := newFeed(t)

	archived := f.ticketWith(t, "Narwhal archived", "")
	f.ticketWith(t, "Narwhal live", "")

	if rec := call(t, f.mux, http.MethodDelete, V1+"/tickets/"+archived.ID, nil, f.willC); rec.Code >= 300 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}

	hits := f.search(t, f.willC, map[string]any{"q": "narwhal"})
	if len(hits) != 1 || hits[0].Title != "Narwhal live" {
		t.Errorf("hits = %+v, want only the live ticket", hits)
	}

	// Carol is in another workspace and sees none of it, projects included.
	if got := f.search(t, f.carolC, map[string]any{"q": "narwhal"}); len(got) != 0 {
		t.Errorf("another workspace found %v", ids(got))
	}
	if got := f.search(t, f.carolC, map[string]any{"q": "core"}); len(got) != 0 {
		t.Errorf("another workspace found the project: %v", ids(got))
	}
}

func TestSearchRejectsABadQuery(t *testing.T) {
	f := newFeed(t)

	for _, tc := range []struct {
		name string
		body map[string]any
		loc  string
	}{
		{"empty q", map[string]any{"q": ""}, "body.q"},
		{"blank q", map[string]any{"q": "   \t"}, "body.q"},
		{"missing q", map[string]any{}, "body"},
		{"q over 200 characters", map[string]any{"q": strings.Repeat("a", 201)}, "body.q"},
		{"limit over the maximum", map[string]any{"q": "x", "limit": 51}, "body.limit"},
		{"limit zero", map[string]any{"q": "x", "limit": 0}, "body.limit"},
		{"unknown type", map[string]any{"q": "x", "type": "epic"}, "body.type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(t, f.mux, http.MethodPost, V1+"/search/list", tc.body, f.willC)
			wantDocProblem(t, rec, http.StatusUnprocessableEntity, "unprocessable_entity", tc.loc)
		})
	}

	// Syntax a person might type must never be a server error.
	for _, q := range []string{`"unclosed`, `a & | !b`, `-`, `the`, `'; DROP TABLE x; --`, `((`} {
		if rec := call(t, f.mux, http.MethodPost, V1+"/search/list", map[string]any{"q": q}, f.willC); rec.Code != http.StatusOK {
			t.Errorf("q=%q: status %d, want 200 - %.200s", q, rec.Code, rec.Body)
		}
	}
}

// The ticket's "a rebuild matches the live index", asserted through the endpoint: every
// source is written live, the answers are noted, the index is thrown away and rebuilt,
// and the same questions get the same answers.
func TestSearchAfterARebuildAnswersAsTheLiveIndexDid(t *testing.T) {
	f := newFeed(t)

	f.ticketWith(t, "Heron migration", "moves the heron colony")
	typeID, fields := f.aspectType(t, f.willC, "Habitat")
	f.document(t, f.willC, map[string]any{
		"title": "Heron field guide", "type": "aspect", "aspect_type_id": typeID,
		"body": map[string]any{fields[0]: "wetlands near the heron rookery"},
	})

	live := f.search(t, f.willC, map[string]any{"q": "heron"})
	liveCore := f.search(t, f.willC, map[string]any{"q": "core"})
	if len(live) != 2 || len(liveCore) != 1 {
		t.Fatalf("live: %v / %v", ids(live), ids(liveCore))
	}

	if _, err := search.NewService(f.pool).Rebuild(testCtx(t)); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	for name, pair := range map[string][2][]searchHitWire{
		"heron": {live, f.search(t, f.willC, map[string]any{"q": "heron"})},
		"core":  {liveCore, f.search(t, f.willC, map[string]any{"q": "core"})},
	} {
		before, after := pair[0], pair[1]
		if len(before) != len(after) {
			t.Fatalf("%s: %v before, %v after", name, ids(before), ids(after))
		}
		for i := range before {
			if before[i].ID != after[i].ID || before[i].Snippet != after[i].Snippet ||
				before[i].Score != after[i].Score || before[i].Title != after[i].Title {
				t.Errorf("%s[%d]: %+v before, %+v after", name, i, before[i], after[i])
			}
		}
	}
}

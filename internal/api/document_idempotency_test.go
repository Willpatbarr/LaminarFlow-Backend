package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-126. Idempotency-Key on POST /documents, the wire half. The rules are the ticket's
// (idempotency_test.go); these prove the document create honours them.

func (f feed) keyedDocument(t *testing.T, cookie, key string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/documents", body, cookie, "Idempotency-Key", key)
}

func (f feed) liveDocuments(t *testing.T, cookie string) int {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/documents/list", map[string]any{}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list documents: %d %.200s", rec.Code, rec.Body)
	}
	var list struct {
		Items []documentWire `json:"items"`
	}
	decode(t, rec, &list)

	return len(list.Items)
}

func TestTheSameKeyTwiceIsOneDocumentAndOneEvent(t *testing.T) {
	f := newFeed(t)
	watcher := f.open(t, f.cookie(f.sarahC))
	body := map[string]any{"title": "Spec", "project_id": f.project.ID, "body": map[string]any{"content": "# hi"}}

	first := f.keyedDocument(t, f.willC, "doc-1", body)
	second := f.keyedDocument(t, f.willC, "doc-1", body)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d, want 200 for both - %.200s", first.Code, second.Code, second.Body)
	}
	if got := first.Header().Get("Idempotent-Replayed"); got != "false" {
		t.Errorf("first Idempotent-Replayed = %q, want false", got)
	}
	if got := second.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("second Idempotent-Replayed = %q, want true", got)
	}

	var a, b documentWire
	decode(t, first, &a)
	decode(t, second, &b)
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("ids %q and %q, want the same", a.ID, b.ID)
	}
	if second.Header().Get("ETag") != `"1"` {
		t.Errorf("replay ETag = %q", second.Header().Get("ETag"))
	}
	if n := f.liveDocuments(t, f.willC); n != 1 {
		t.Errorf("%d documents, want 1", n)
	}

	// Exactly one document.created: after a marker write, the next frame being the marker
	// proves nothing came between.
	if ev := readEvent(t, watcher); ev.Type != "document.created" || ev.Document["id"] != a.ID {
		t.Errorf("first frame = %+v, want document.created for %s", ev, a.ID)
	}
	marker := f.document(t, f.willC, map[string]any{"title": "marker", "project_id": f.project.ID})
	if ev := readEvent(t, watcher); ev.Type != "document.created" || ev.Document["id"] != marker.ID {
		t.Errorf("second frame = %+v, want the marker's document.created, not a replay of the first", ev)
	}
}

func TestADocumentReplayReturnsTheCurrentCopyAndIgnoresItsBody(t *testing.T) {
	f := newFeed(t)

	var made documentWire
	decode(t, f.keyedDocument(t, f.willC, "k", map[string]any{"title": "v1"}), &made)
	call(t, f.mux, http.MethodPut, V1+"/documents/"+made.ID, map[string]any{"title": "v2"}, f.willC)

	rec := f.keyedDocument(t, f.willC, "k", map[string]any{"title": "something else"})
	var got documentWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.ID != made.ID || got.Title != "v2" || got.Version != 2 {
		t.Errorf("replay = %d %+v, want the original at v2 titled v2", rec.Code, got)
	}
}

func TestADocumentWithoutAKeyHasNoReplayHeader(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{"title": "t"}, f.willC)
	if _, present := rec.Header()["Idempotent-Replayed"]; present {
		t.Error("Idempotent-Replayed sent on a create with no key")
	}
}

func TestTheSameDocumentKeyFromAnotherAccountIsAnotherDocument(t *testing.T) {
	f := newFeed(t)

	var a, b documentWire
	decode(t, f.keyedDocument(t, f.willC, "shared", map[string]any{"title": "will's"}), &a)
	rec := f.keyedDocument(t, f.sarahC, "shared", map[string]any{"title": "sarah's"})
	decode(t, rec, &b)

	if rec.Header().Get("Idempotent-Replayed") != "false" || a.ID == b.ID || b.Title != "sarah's" {
		t.Errorf("second account got %+v (replayed %q), want its own new document", b, rec.Header().Get("Idempotent-Replayed"))
	}
}

func TestSixteenConcurrentDocumentCreatesWithOneKeyMakeOneDocument(t *testing.T) {
	f := newFeed(t)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.keyedDocument(t, f.willC, "race", map[string]any{"title": "t"})
			var d documentWire
			decode(t, rec, &d)
			mu.Lock()
			ids[d.ID] = true
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
	if n := f.liveDocuments(t, f.willC); n != 1 {
		t.Errorf("%d documents, want 1", n)
	}
}

func TestAMalformedDocumentKeyIsA422OnTheHeader(t *testing.T) {
	f := newFeed(t)

	for name, key := range map[string]string{
		"empty":     "",
		"129 chars": strings.Repeat("k", 129),
		"non-ASCII": "clé-1",
	} {
		refused(t, f.keyedDocument(t, f.willC, key, map[string]any{"title": "t"}), "header.Idempotency-Key")
		if t.Failed() {
			t.Fatalf("%s: stopping at the first failure", name)
		}
	}
	if n := f.liveDocuments(t, f.willC); n != 0 {
		t.Errorf("%d documents stored by malformed keys", n)
	}
}

// Archived since: 409 document_conflict carrying the document, so the client has no reason
// to create another.
func TestADocumentReplayAfterArchiveIsA409CarryingTheDocument(t *testing.T) {
	f := newFeed(t)

	var made documentWire
	decode(t, f.keyedDocument(t, f.willC, "k", map[string]any{"title": "Spec"}), &made)
	if rec := call(t, f.mux, http.MethodDelete, V1+"/documents/"+made.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("archive: %d", rec.Code)
	}

	rec := f.keyedDocument(t, f.willC, "k", map[string]any{"title": "Spec"})
	var c documentConflictWire
	decode(t, rec, &c)
	if rec.Code != http.StatusConflict || c.Code != CodeDocumentConflict || c.Current.ID != made.ID ||
		c.Current.Version != 2 || c.Current.ArchivedAt == nil {
		t.Fatalf("got %d %+v, want 409 document_conflict carrying %s archived at version 2", rec.Code, c, made.ID)
	}
	if n := f.liveDocuments(t, f.willC); n != 0 {
		t.Errorf("%d live documents, want none: the replay must not create a second", n)
	}

	// Restore it and the key is live again.
	if rec := call(t, f.mux, http.MethodPost, V1+"/documents/"+made.ID+"/unarchive", nil, f.willC, "If-Match", `"2"`); rec.Code != http.StatusNoContent {
		t.Fatalf("unarchive: %d", rec.Code)
	}
	if rec := f.keyedDocument(t, f.willC, "k", map[string]any{"title": "Spec"}); rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("after restore: status %d replayed %q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
}

func TestTheOpenAPIDocumentsTheDocumentKeyAndReplay(t *testing.T) {
	api := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()
	op := api.Paths[V1+"/documents"].Post
	if op == nil {
		t.Fatal("no POST /documents in the document")
	}

	var header bool
	for _, p := range op.Parameters {
		if p.In == "header" && p.Name == "Idempotency-Key" {
			header = true
			if p.Required {
				t.Error("Idempotency-Key is required; omitting it must stay legal")
			}
			if p.Schema == nil || p.Schema.MaxLength == nil || *p.Schema.MaxLength != ticket.MaxIdempotencyKeyLength {
				t.Errorf("Idempotency-Key schema = %+v, want maxLength %d", p.Schema, ticket.MaxIdempotencyKeyLength)
			}
		}
	}
	if !header {
		t.Error("create-document does not document Idempotency-Key")
	}
	if ok := op.Responses["200"]; ok == nil || ok.Headers["Idempotent-Replayed"] == nil {
		t.Error("the 200 does not document Idempotent-Replayed")
	}
	if c := op.Responses["409"]; c == nil || c.Content["application/problem+json"].Schema.Ref != "#/components/schemas/DocumentConflict" {
		t.Error("create-document does not document its 409 as DocumentConflict")
	}
}

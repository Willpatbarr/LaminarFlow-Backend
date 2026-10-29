package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-75. Idempotency-Key on the wire: the header, the replay marker, the change feed, and
// the document. The race is internal/ticket's TestConcurrentCreatesWithOneKeyMakeOneTicket;
// the one here goes through HTTP as well.

func (f feed) keyed(t *testing.T, cookie, key string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/tickets", body, cookie, "Idempotency-Key", key)
}

func TestTheSameKeyTwiceIsOneTicketAndOneEvent(t *testing.T) {
	f := newFeed(t)
	watcher := f.open(t, f.cookie(f.sarahC))

	first := f.keyed(t, f.willC, "create-1", f.minimal("Fix login"))
	second := f.keyed(t, f.willC, "create-1", f.minimal("Fix login"))

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses %d and %d, want 200 for both - %.200s", first.Code, second.Code, second.Body)
	}
	if got := first.Header().Get("Idempotent-Replayed"); got != "false" {
		t.Errorf("first Idempotent-Replayed = %q, want false", got)
	}
	if got := second.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("second Idempotent-Replayed = %q, want true", got)
	}

	var a, b ticketWire
	decode(t, first, &a)
	decode(t, second, &b)
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("ids %q and %q, want the same", a.ID, b.ID)
	}
	if second.Header().Get("ETag") != `"1"` {
		t.Errorf("replay ETag = %q", second.Header().Get("ETag"))
	}
	if n := len(f.live(t, f.willC)); n != 1 {
		t.Errorf("%d tickets, want 1", n)
	}

	// Exactly one ticket.created on the feed. Frames arrive in order, so after a marker
	// write the next frame being the marker proves nothing came between.
	expectEvent(t, readEvent(t, watcher), "ticket.created", a.ID, 1, f.will.AccountID)
	marker := f.create(t, f.willC, "marker")
	expectEvent(t, readEvent(t, watcher), "ticket.created", marker.ID, 1, f.will.AccountID)

	// The marker's own log entry follows it (LAM-89); the replay made none.
	if ev := readEventAny(t, watcher); ev.Type != "activity.created" || ev.TicketID != marker.ID {
		t.Errorf("event after the marker = %+v, want its activity.created", ev)
	}

	// And nothing further is waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, data, err := watcher.Read(ctx); err == nil {
		t.Errorf("an extra frame arrived: %s", data)
	}
}

func TestAReplayReturnsTheCurrentTicketAndIgnoresItsBody(t *testing.T) {
	f := newFeed(t)

	var made ticketWire
	decode(t, f.keyed(t, f.willC, "k", f.minimal("Original")), &made)
	if rec := f.put(t, f.willC, made.ID, replace("Edited", nil)); rec.Code != http.StatusOK {
		t.Fatalf("update: %d", rec.Code)
	}

	rec := f.keyed(t, f.willC, "k", f.minimal("A different ticket"))
	var replay ticketWire
	decode(t, rec, &replay)
	if replay.ID != made.ID || replay.Title != "Edited" || replay.Version != 2 {
		t.Errorf("replay = %+v, want the original ticket as it is now", replay)
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("not marked as a replay")
	}
}

func TestWithoutAKeyThereIsNoReplayHeader(t *testing.T) {
	f := newFeed(t)

	rec := f.post(t, f.willC, f.minimal("t"))
	if _, present := rec.Header()["Idempotent-Replayed"]; present {
		t.Errorf("Idempotent-Replayed present without a key: %v", rec.Header()["Idempotent-Replayed"])
	}
}

func TestTheSameKeyFromAnotherAccountIsAnotherTicket(t *testing.T) {
	f := newFeed(t)

	var a, b ticketWire
	decode(t, f.keyed(t, f.willC, "shared", f.minimal("Will's")), &a)
	rec := f.keyed(t, f.sarahC, "shared", f.minimal("Sarah's"))
	decode(t, rec, &b)

	if a.ID == b.ID {
		t.Error("one account's key returned another's ticket")
	}
	if rec.Header().Get("Idempotent-Replayed") != "false" {
		t.Error("the second account's first use was marked a replay")
	}
	if b.Title != "Sarah's" {
		t.Errorf("title %q, want her own", b.Title)
	}
	if n := len(f.live(t, f.willC)); n != 2 {
		t.Errorf("%d tickets, want 2", n)
	}
}

func TestSixteenConcurrentCreatesWithOneKeyMakeOneTicket(t *testing.T) {
	f := newFeed(t)
	watcher := f.open(t, f.cookie(f.sarahC))

	const n = 16
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		ids     = map[string]int{}
		replays int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			rec := f.keyed(t, f.willC, "race", f.minimal("Fix login"))

			mu.Lock()
			defer mu.Unlock()
			if rec.Code != http.StatusOK {
				t.Errorf("status %d - %.200s", rec.Code, rec.Body)
				return
			}
			var got ticketWire
			decode(t, rec, &got)
			ids[got.ID]++
			if rec.Header().Get("Idempotent-Replayed") == "true" {
				replays++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(ids) != 1 || replays != n-1 {
		t.Errorf("ids %v, replays %d: want one id and %d replays", ids, replays, n-1)
	}
	if got := len(f.live(t, f.willC)); got != 1 {
		t.Errorf("%d tickets, want 1", got)
	}

	// One event for the one create, then the marker.
	for id := range ids {
		expectEvent(t, readEvent(t, watcher), "ticket.created", id, 1, f.will.AccountID)
	}
	marker := f.create(t, f.willC, "marker")
	expectEvent(t, readEvent(t, watcher), "ticket.created", marker.ID, 1, f.will.AccountID)
}

func TestAMalformedKeyIsA422OnTheHeader(t *testing.T) {
	f := newFeed(t)

	for name, key := range map[string]string{
		"empty":         "",
		"129 chars":     strings.Repeat("k", 129),
		"non-ASCII":     "clé-1",
		"tab":           "a\tb",
		"emoji":         "🙂",
		"control (DEL)": "a\x7fb",
	} {
		rec := f.keyed(t, f.willC, key, f.minimal("t"))
		refused(t, rec, "header.Idempotency-Key")
		if t.Failed() {
			t.Fatalf("%s: stopping at the first failure", name)
		}
	}

	// The header sent twice is ambiguous.
	req := httptest.NewRequest(http.MethodPost, V1+"/tickets", strings.NewReader(`{"project_id":"`+f.project.ID+`","title":"t"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("Idempotency-Key", "a")
	req.Header.Add("Idempotency-Key", "b")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.willC})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("the header sent twice was accepted - %.200s", rec.Body)
	}

	if n := len(f.live(t, f.willC)); n != 0 {
		t.Errorf("%d tickets stored by malformed keys", n)
	}

	// The extremes that are fine.
	for _, key := range []string{"k", strings.Repeat("k", 128), "with spaces ~!#", `"quoted"`} {
		if rec := f.keyed(t, f.willC, key, f.minimal("t")); rec.Code != http.StatusOK {
			t.Errorf("key %q: status %d - %.200s", key, rec.Code, rec.Body)
		}
	}
}

// Archived since: 409 ticket_archived carrying the ticket, who archived it and when. The
// client learns the ticket exists, so it has no reason to create another.
func TestAReplayAfterArchiveIsA409CarryingTheTicket(t *testing.T) {
	f := newFeed(t)

	var made ticketWire
	decode(t, f.keyed(t, f.willC, "k", f.minimal("Fix login")), &made)
	if rec := call(t, f.mux, http.MethodDelete, V1+"/tickets/"+made.ID, nil, f.sarahC); rec.Code != http.StatusNoContent {
		t.Fatalf("archive: %d", rec.Code)
	}

	rec := f.keyed(t, f.willC, "k", f.minimal("Fix login"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 - %.300s", rec.Code, rec.Body)
	}

	var c conflictWire
	decode(t, rec, &c)
	if c.Code != CodeTicketArchived || c.Current.ID != made.ID || c.Current.Version != 2 {
		t.Errorf("conflict = %+v, want ticket_archived carrying %s at version 2", c, made.ID)
	}
	if c.ChangedBy == nil || c.ChangedBy.AccountID != f.sarah.AccountID {
		t.Errorf("changed_by = %+v, want the archiver", c.ChangedBy)
	}
	if c.ChangedAt.IsZero() {
		t.Error("changed_at missing")
	}
	// The original is archived, so it is not listed; a duplicate would be.
	if n := len(f.live(t, f.willC)); n != 0 {
		t.Errorf("%d live tickets, want none: the replay must not create a second", n)
	}

	// The client can act on it: restore at the version it was told, and the key is live again.
	restore := call(t, f.mux, http.MethodPost, V1+"/tickets/"+made.ID+"/unarchive", nil, f.willC, "If-Match", `"2"`)
	if restore.Code != http.StatusNoContent {
		t.Fatalf("unarchive: %d", restore.Code)
	}
	if rec := f.keyed(t, f.willC, "k", f.minimal("Fix login")); rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("after restore: status %d replayed %q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
}

func TestAnInvalidBodyIsStillRefusedOnAReplay(t *testing.T) {
	f := newFeed(t)
	f.keyed(t, f.willC, "k", f.minimal("Fix login"))

	refused(t, f.keyed(t, f.willC, "k", f.minimal("   ")), "body.title")
}

func TestTheOpenAPIDocumentsTheKeyAndTheReplay(t *testing.T) {
	api := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).OpenAPI()
	op := api.Paths[V1+"/tickets"].Post
	if op == nil {
		t.Fatal("no POST /tickets in the document")
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
		t.Error("create-ticket does not document Idempotency-Key")
	}

	ok := op.Responses["200"]
	if ok == nil || ok.Headers["Idempotent-Replayed"] == nil {
		t.Error("the 200 does not document Idempotent-Replayed")
	}

	conflict := op.Responses["409"]
	if conflict == nil || conflict.Content["application/problem+json"].Schema.Ref != "#/components/schemas/TicketConflict" {
		t.Error("create-ticket does not document its 409 as TicketConflict")
	}
	if op.Responses["default"] == nil {
		t.Error("create-ticket lost its default error response")
	}
}

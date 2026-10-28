package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// LAM-111. The wire half of ticket-document links: statuses, idempotency, both reads, and the
// change-feed events.

type linkedDocWire struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Type         string     `json:"type"`
	AspectTypeID *string    `json:"aspect_type_id"`
	ArchivedAt   *time.Time `json:"archived_at"`
}

type linkedTicketWire struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	ArchivedAt *time.Time `json:"archived_at"`
}

func (f feed) link(t *testing.T, cookie, ticketID, documentID string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodPost, V1+"/tickets/"+ticketID+"/documents", map[string]string{"document_id": documentID}, cookie)
}

func (f feed) unlink(t *testing.T, cookie, ticketID, documentID string) *httptest.ResponseRecorder {
	t.Helper()

	return call(t, f.mux, http.MethodDelete, V1+"/tickets/"+ticketID+"/documents/"+documentID, nil, cookie)
}

func (f feed) ticketDocs(t *testing.T, ticketID string) []linkedDocWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/tickets/"+ticketID, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get ticket: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Documents []linkedDocWire `json:"documents"`
	}
	decode(t, rec, &got)
	if got.Documents == nil {
		t.Fatalf("documents is null, want an array: %s", rec.Body)
	}

	return got.Documents
}

func (f feed) docTickets(t *testing.T, documentID string) []linkedTicketWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodGet, V1+"/documents/"+documentID, nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("get document: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Tickets []linkedTicketWire `json:"tickets"`
	}
	decode(t, rec, &got)
	if got.Tickets == nil {
		t.Fatalf("tickets is null, want an array: %s", rec.Body)
	}

	return got.Tickets
}

func TestALinkListsFromBothSidesAndIsIdempotent(t *testing.T) {
	f := newFeed(t)

	tk := f.create(t, f.willC, "Fix login")
	other := f.create(t, f.willC, "Second")
	d := f.document(t, f.willC, map[string]any{"title": "Login design", "project_id": f.project.ID})

	if got := f.ticketDocs(t, tk.ID); len(got) != 0 {
		t.Fatalf("a fresh ticket lists %+v", got)
	}

	for i := 0; i < 2; i++ { // the repeat is also a 204
		if rec := f.link(t, f.sarahC, tk.ID, d.ID); rec.Code != http.StatusNoContent {
			t.Fatalf("link #%d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := f.link(t, f.willC, other.ID, d.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("second ticket: %d %s", rec.Code, rec.Body)
	}

	docs := f.ticketDocs(t, tk.ID)
	if len(docs) != 1 || docs[0].ID != d.ID || docs[0].Title != "Login design" || docs[0].Type != "normal" ||
		docs[0].AspectTypeID != nil || docs[0].ArchivedAt != nil {
		t.Errorf("ticket's documents = %+v", docs)
	}

	tickets := f.docTickets(t, d.ID)
	if len(tickets) != 2 || tickets[0].ID != tk.ID || tickets[0].Title != "Fix login" || tickets[1].ID != other.ID {
		t.Errorf("document's tickets = %+v, want both, oldest link first", tickets)
	}

	for i := 0; i < 2; i++ {
		if rec := f.unlink(t, f.willC, tk.ID, d.ID); rec.Code != http.StatusNoContent {
			t.Fatalf("unlink #%d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if got := f.ticketDocs(t, tk.ID); len(got) != 0 {
		t.Errorf("after unlink the ticket lists %+v", got)
	}
	if got := f.docTickets(t, d.ID); len(got) != 1 || got[0].ID != other.ID {
		t.Errorf("after unlink the document lists %+v, want only the other ticket", got)
	}

	// Never linked at all: still 204.
	third := f.create(t, f.willC, "Third")
	if rec := f.unlink(t, f.willC, third.ID, d.ID); rec.Code != http.StatusNoContent {
		t.Errorf("unlink of a pair never linked: %d %s", rec.Code, rec.Body)
	}
}

func TestAnAspectDocumentListsItsTypeOnTheTicket(t *testing.T) {
	f := newFeed(t)

	typeID, fields := f.aspectType(t, f.willC, "Summary")
	d := f.document(t, f.willC, map[string]any{
		"title": "Spec", "type": "aspect", "aspect_type_id": typeID, "body": map[string]string{fields[0]: "x"},
	})
	tk := f.create(t, f.willC, "T")
	f.link(t, f.willC, tk.ID, d.ID)

	docs := f.ticketDocs(t, tk.ID)
	if len(docs) != 1 || docs[0].Type != "aspect" || docs[0].AspectTypeID == nil || *docs[0].AspectTypeID != typeID {
		t.Errorf("documents = %+v", docs)
	}
}

func TestArchivingEitherEndKeepsTheLink(t *testing.T) {
	f := newFeed(t)

	tk := f.create(t, f.willC, "Old")
	d := f.document(t, f.willC, map[string]any{"title": "Notes", "project_id": f.project.ID})
	late := f.document(t, f.willC, map[string]any{"title": "Late", "project_id": f.project.ID})
	f.link(t, f.willC, tk.ID, d.ID)

	// Archive the document: the ticket still lists it, with archived_at set.
	if rec := call(t, f.mux, http.MethodDelete, V1+"/documents/"+d.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("archive document: %d", rec.Code)
	}
	docs := f.ticketDocs(t, tk.ID)
	if len(docs) != 1 || docs[0].ArchivedAt == nil {
		t.Fatalf("after archiving the document: %+v", docs)
	}

	// Archive the ticket, then link to it and from it: an archived ticket still links.
	if rec := call(t, f.mux, http.MethodDelete, V1+"/tickets/"+tk.ID, nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("archive ticket: %d", rec.Code)
	}
	if rec := f.link(t, f.willC, tk.ID, late.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("link on an archived ticket: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/documents/"+d.ID+"/unarchive", nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("unarchive document: %d", rec.Code)
	}
	if rec := call(t, f.mux, http.MethodPost, V1+"/tickets/"+tk.ID+"/unarchive", nil, f.willC); rec.Code != http.StatusNoContent {
		t.Fatalf("unarchive ticket: %d", rec.Code)
	}

	docs = f.ticketDocs(t, tk.ID)
	if len(docs) != 2 || docs[0].ID != d.ID || docs[0].ArchivedAt != nil || docs[1].ID != late.ID {
		t.Errorf("after unarchiving both: %+v", docs)
	}

	// An archived ticket is listed on the document's side with its archived_at.
	call(t, f.mux, http.MethodDelete, V1+"/tickets/"+tk.ID, nil, f.willC)
	tickets := f.docTickets(t, d.ID)
	if len(tickets) != 1 || tickets[0].ArchivedAt == nil {
		t.Errorf("document's tickets with the ticket archived = %+v", tickets)
	}
	// Unlinking works on an archived ticket too.
	if rec := f.unlink(t, f.willC, tk.ID, d.ID); rec.Code != http.StatusNoContent {
		t.Errorf("unlink on an archived ticket: %d", rec.Code)
	}
	if got := f.docTickets(t, d.ID); len(got) != 0 {
		t.Errorf("link survived the unlink: %+v", got)
	}
}

func TestLinkRefusalsAreNotFound(t *testing.T) {
	f := newFeed(t)

	tk := f.create(t, f.willC, "Mine")
	d := f.document(t, f.willC, map[string]any{"title": "Mine"})
	foreign := f.document(t, f.carolC, map[string]any{"title": "Elsewhere"})
	const unknown = "00000000-0000-4000-8000-000000000001"

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"unknown ticket":         f.link(t, f.willC, unknown, d.ID),
		"unknown document":       f.link(t, f.willC, tk.ID, unknown),
		"document of another ws": f.link(t, f.willC, tk.ID, foreign.ID),
		"caller outside the ws":  f.link(t, f.carolC, tk.ID, foreign.ID),
		"unlink unknown ticket":  f.unlink(t, f.willC, unknown, d.ID),
		"unlink foreign caller":  f.unlink(t, f.carolC, tk.ID, d.ID),
	} {
		var p problemWire
		decode(t, rec, &p)
		if rec.Code != http.StatusNotFound || p.Code != "not_found" {
			t.Errorf("%s: got %d %q (%s), want 404 not_found", name, rec.Code, p.Code, rec.Body)
		}
	}

	if got := f.ticketDocs(t, tk.ID); len(got) != 0 {
		t.Errorf("a refused link was stored: %+v", got)
	}

	// The document's side hides the tickets from a caller of another workspace.
	f.link(t, f.willC, tk.ID, d.ID)
	if rec := call(t, f.mux, http.MethodGet, V1+"/documents/"+d.ID, nil, f.carolC); rec.Code != http.StatusNotFound {
		t.Errorf("foreign read of the document: %d", rec.Code)
	}
}

func TestLinkAndUnlinkPublishOnlyWhenTheyChangeSomething(t *testing.T) {
	f := newFeed(t)

	tk := f.create(t, f.willC, "T")
	d := f.document(t, f.willC, map[string]any{"title": "D"})

	conn := f.open(t, f.cookie(f.sarahC))
	read := func() (ev struct {
		Type       string `json:"type"`
		TicketID   string `json:"ticket_id"`
		DocumentID string `json:"document_id"`
		ActorID    string `json:"actor_id"`
		Version    int    `json:"version"`
	}) {
		t.Helper()
		kind, data, err := conn.Read(testCtx(t))
		if err != nil || kind != 1 {
			t.Fatalf("read: %v", err)
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("event %s: %v", data, err)
		}
		return ev
	}

	f.link(t, f.willC, tk.ID, d.ID)
	f.link(t, f.willC, tk.ID, d.ID) // repeat: no event
	f.unlink(t, f.willC, tk.ID, d.ID)
	f.unlink(t, f.willC, tk.ID, d.ID) // repeat: no event
	f.link(t, f.willC, tk.ID, d.ID)

	for i, want := range []string{"ticket.document_linked", "ticket.document_unlinked", "ticket.document_linked"} {
		ev := read()
		if ev.Type != want || ev.TicketID != tk.ID || ev.DocumentID != d.ID || ev.ActorID != f.will.AccountID || ev.Version != 0 {
			t.Errorf("event %d = %+v, want %s for the pair", i, ev, want)
		}
	}
}

func TestTheLinkOperationsAreInTheContract(t *testing.T) {
	for _, name := range []string{"LinkedDocumentBody", "LinkedTicketBody"} {
		schemaNamed(t, name)
	}
}

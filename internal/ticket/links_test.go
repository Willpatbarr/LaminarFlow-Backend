package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-111. The link rules at the service; the wire half is internal/api's ticket_document_test.go.

func (w world) doc(t *testing.T, account, title string) document.Document {
	t.Helper()

	d, err := document.NewService(w.pool).Write(context.Background(), document.SaveParams{AccountID: account, Title: title})
	if err != nil {
		t.Fatalf("document: %v", err)
	}

	return d
}

func TestLinkIsScopedToTheCallersWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	tk := w.create(t, "T")
	d := w.doc(t, w.member, "D")
	foreign := w.doc(t, w.outsid, "Elsewhere") // the outsider's workspace is not the ticket's

	if _, err := w.svc.LinkDocument(ctx, w.member, tk.ID, foreign.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("link across workspaces: %v, want ErrNotFound", err)
	}
	if _, err := w.svc.LinkDocument(ctx, w.outsid, tk.ID, foreign.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("link by a non-member: %v, want ErrNotFound", err)
	}
	if _, err := w.svc.UnlinkDocument(ctx, w.outsid, tk.ID, d.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("unlink by a non-member: %v, want ErrNotFound", err)
	}

	first, err := w.svc.LinkDocument(ctx, w.member, tk.ID, d.ID)
	if err != nil || !first.Changed || first.ProjectID != w.project {
		t.Fatalf("first link = %+v, %v", first, err)
	}
	again, err := w.svc.LinkDocument(ctx, w.member, tk.ID, d.ID)
	if err != nil || again.Changed {
		t.Fatalf("repeat link = %+v, %v, want Changed false", again, err)
	}

	if got, _ := w.svc.LinkedTickets(ctx, w.outsid, d.ID); len(got) != 0 {
		t.Errorf("a non-member sees the document's tickets: %+v", got)
	}
}

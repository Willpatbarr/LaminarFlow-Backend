/*
╔═ ticket.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · ticket resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeTicketNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · get · list · update · archive · unarchive
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
)

// One 404 for absent, archived, and not-yours alike - the service collapses them on
// purpose, and a distinct code here would undo that by telling a caller which it hit.
const CodeTicketNotFound = "ticket_not_found"

// This file is the template the other five resources copy, so its shape matters as
// much as its behaviour: one input struct per verb, handlers that parse and format and
// nothing else, every operation marked RequireAuth, and every service call passed the
// caller's account.

type ticketBody struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	StatusID    *string `json:"status_id" doc:"Null is normal - no status is sacred, so a ticket may have none."`
	AssigneeID  *string `json:"assignee_id" doc:"Null is normal - an unassigned ticket."`
}

type ticketOutput struct {
	Body ticketBody
}

type createTicketInput struct {
	Body struct {
		ProjectID   string  `json:"project_id" required:"true" format:"uuid"`
		Title       string  `json:"title" required:"true"`
		Description string  `json:"description"`
		StatusID    *string `json:"status_id" format:"uuid"`
		AssigneeID  *string `json:"assignee_id" format:"uuid"`
	}
}

type getTicketInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateTicketInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Title       string  `json:"title" required:"true"`
		Description string  `json:"description"`
		StatusID    *string `json:"status_id" format:"uuid"`
		AssigneeID  *string `json:"assignee_id" format:"uuid"`
	}
}

func asBody(t ticket.Ticket) ticketBody {
	return ticketBody{
		ID:          t.ID,
		ProjectID:   t.ProjectID,
		Title:       t.Title,
		Description: t.Description,
		StatusID:    t.StatusID,
		AssigneeID:  t.AssigneeID,
	}
}

// ticketError maps the service's one error onto the envelope. Anything else is a
// fault on this side and must not be reported as a 404, or a caller retries forever
// against a database that is down.
func ticketError(err error) error {
	if errors.Is(err, ticket.ErrNotFound) {
		return NewError(http.StatusNotFound, CodeTicketNotFound, "no such ticket")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the five ticket operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *ticket.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/tickets, GET /api/v1/tickets/{id}
*/

func registerTickets(api huma.API, svc *ticket.Service) {
	// Every operation requires a caller. There is no public ticket endpoint, and
	// marking them individually rather than wrapping the group means a new one is
	// unprotected only if someone omits the line - which the middleware test catches.
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets",
		Summary:     "Create a ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *createTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Create(ctx, me.AccountID, ticket.CreateParams{
			ProjectID:   in.Body.ProjectID,
			Title:       in.Body.Title,
			Description: in.Body.Description,
			StatusID:    in.Body.StatusID,
			AssigneeID:  in.Body.AssigneeID,
		})
		if err != nil {
			return nil, ticketError(err)
		}

		return &ticketOutput{Body: asBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-ticket",
		Method:      http.MethodGet,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Fetch one ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *getTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, ticketError(err)
		}

		return &ticketOutput{Body: asBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-ticket",
		Method:      http.MethodPut,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Replace a ticket's writable fields",
		Metadata:    requires,
	}, func(ctx context.Context, in *updateTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		// PUT, not PATCH: UpdateParams replaces wholesale, and the method should say
		// so. A caller omitting status_id clears it, which is what PUT means.
		t, err := svc.Update(ctx, me.AccountID, ticket.UpdateParams{
			ID:          in.ID,
			Title:       in.Body.Title,
			Description: in.Body.Description,
			StatusID:    in.Body.StatusID,
			AssigneeID:  in.Body.AssigneeID,
		})
		if err != nil {
			return nil, ticketError(err)
		}

		return &ticketOutput{Body: asBody(t)}, nil
	})

	// DELETE archives. The verb is what a client expects for "remove this", and 0029
	// is why the row survives - a hard delete CASCADEs to comment and takes the
	// discussion with it. The summary says so, because a DELETE that does not delete
	// is exactly the sort of thing a reader should not have to discover.
	huma.Register(api, huma.Operation{
		OperationID: "archive-ticket",
		Method:      http.MethodDelete,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Archive a ticket (soft delete - the row and its comments survive)",
		Metadata:    requires,
	}, func(ctx context.Context, in *getTicketInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.Archive(ctx, me.AccountID, in.ID); err != nil {
			return nil, ticketError(err)
		}

		return nil, nil
	})

	// The list operation is registered through the generic path rather than spelled
	// out here: the body, the envelope and the error mapping are the same for every
	// resource, and only the vocabulary and the row mapping are ticket's.
	registerList(api, listSpec[ticketBody]{
		OperationID: "list-tickets",
		Path:        V1 + "/tickets/list",
		Summary:     "List tickets (a read - see the description for why it is a POST)",
		Fields:      ticket.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[ticketBody], error) {
			p := ticket.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.List(ctx, accountID, p)
			if err != nil {
				return listPage[ticketBody]{}, err
			}

			items := make([]ticketBody, len(page.Tickets))
			for i, t := range page.Tickets {
				items[i] = asBody(t)
			}

			return listPage[ticketBody]{
				Items:      items,
				NextCursor: page.NextCursor,
				HasMore:    page.HasMore,
			}, nil
		},
	})

	huma.Register(api, huma.Operation{
		OperationID: "unarchive-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets/{id}/unarchive",
		Summary:     "Restore an archived ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *getTicketInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.Unarchive(ctx, me.AccountID, in.ID); err != nil {
			return nil, ticketError(err)
		}

		return nil, nil
	})
}

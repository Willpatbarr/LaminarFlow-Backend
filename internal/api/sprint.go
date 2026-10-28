/*
╔═ sprint.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · sprints, and the tickets in them
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeSprintNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  seven operations under /sprints
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sprint"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
)

// CodeSprintNotFound is a 404 for a sprint id in the path that is absent or out of scope.
const CodeSprintNotFound = "sprint_not_found"

type sprintBody struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	StartDate *string   `json:"start_date" format:"date" doc:"YYYY-MM-DD. Null until the sprint is dated."`
	EndDate   *string   `json:"end_date" format:"date" doc:"YYYY-MM-DD, never before start_date. Null until the sprint is dated."`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type sprintOutput struct {
	Body sprintBody
}

type createSprintInput struct {
	Body struct {
		ProjectID string  `json:"project_id" required:"true" format:"uuid"`
		Name      string  `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		StartDate *string `json:"start_date,omitempty" format:"date" doc:"YYYY-MM-DD. Omit, or null, for none."`
		EndDate   *string `json:"end_date,omitempty" format:"date" doc:"YYYY-MM-DD. Before start_date is a 422 at body.end_date. Omit, or null, for none."`
	}
}

type listSprintsInput struct {
	Body struct {
		ProjectID string `json:"project_id" required:"true" format:"uuid"`
	}
}

type sprintInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateSprintInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name      *string   `json:"name,omitempty" minLength:"1" maxLength:"100"`
		StartDate patchDate `json:"start_date,omitempty" doc:"YYYY-MM-DD. Omit to leave it, null to clear it."`
		EndDate   patchDate `json:"end_date,omitempty" doc:"YYYY-MM-DD. Omit to leave it, null to clear it. Before start_date is a 422 at body.end_date."`
	}
}

type sprintTicketInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		TicketID string `json:"ticket_id" required:"true" format:"uuid" doc:"A live ticket of the sprint's project, or a 422 ticket_not_found."`
	}
}

type sprintTicketPathInput struct {
	ID       string `path:"id" format:"uuid"`
	TicketID string `path:"ticket_id" format:"uuid"`
}

func asSprintBody(s sprint.Sprint) sprintBody {
	return sprintBody{ID: s.ID, ProjectID: s.ProjectID, Name: s.Name, StartDate: s.StartDate, EndDate: s.EndDate,
		CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC()}
}

func sprintError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	switch {
	case errors.Is(err, sprint.ErrNotFound), errors.Is(err, ticket.ErrSprintNotFound):
		return NewError(http.StatusNotFound, CodeSprintNotFound, "no such sprint")
	case errors.Is(err, sprint.ErrProjectNotFound):
		return NewError(http.StatusNotFound, CodeProjectNotFound, "no such project")
	case errors.Is(err, sprint.ErrEndBeforeStart):
		return newFieldError(defaultCode(http.StatusUnprocessableEntity), "the sprint ends before it starts",
			"body.end_date", "must not be before start_date")
	case errors.Is(err, ticket.ErrNotInSprintProject):
		return newFieldError(CodeTicketNotFound, "the ticket is not one this sprint's project has",
			"body.ticket_id", "no such ticket in this sprint's project")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the sprint operations
├─ in ────────────────────────────────────────────
│      api        huma.API
│      svc        *sprint.Service     nil only for cmd/openapi
│      tickets    *ticket.Service     owns membership, which is a ticket write
│      feed       ticketFeed          publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/sprints/{id}/tickets
*/

func registerSprints(api huma.API, svc *sprint.Service, tickets *ticket.Service, feed ticketFeed) {

	huma.Register(api, huma.Operation{
		OperationID: "create-sprint",
		Method:      http.MethodPost,
		Path:        V1 + "/sprints",
		Summary:     "Create a sprint in a project",
		Description: "Both dates are optional. An `end_date` before `start_date` is a 422 at `body.end_date`. Announced as `sprint.created`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createSprintInput) (*sprintOutput, error) {
		me, _ := auth.FromContext(ctx)

		s, err := svc.Create(ctx, me.AccountID, in.Body.ProjectID, in.Body.Name, in.Body.StartDate, in.Body.EndDate)
		if err != nil {
			return nil, sprintError(err)
		}

		feed.publishSprint(s.WorkspaceID, realtime.SprintCreated, me.AccountID, s)

		return &sprintOutput{Body: asSprintBody(s)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-sprints",
		Method:      http.MethodPost,
		Path:        V1 + "/sprints/list",
		Summary:     "List a project's sprints (a read - POST for the body)",
		Description: "Soonest start first, undated ones last. Always one page: `next_cursor` is null and `has_more` false. " +
			"Which tickets are in a sprint is read through `POST /tickets/list` with a `sprint` filter.",
		Metadata: needs(auth.TicketRead),
	}, func(ctx context.Context, in *listSprintsInput) (*listOutput[sprintBody], error) {
		me, _ := auth.FromContext(ctx)

		sprints, err := svc.List(ctx, me.AccountID, in.Body.ProjectID)
		if err != nil {
			return nil, sprintError(err)
		}

		out := &listOutput[sprintBody]{}
		out.Body.Items = make([]sprintBody, len(sprints))
		for i, s := range sprints {
			out.Body.Items[i] = asSprintBody(s)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-sprint",
		Method:      http.MethodGet,
		Path:        V1 + "/sprints/{id}",
		Summary:     "Fetch one sprint",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *sprintInput) (*sprintOutput, error) {
		me, _ := auth.FromContext(ctx)

		s, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, sprintError(err)
		}

		return &sprintOutput{Body: asSprintBody(s)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-sprint",
		Method:      http.MethodPut,
		Path:        V1 + "/sprints/{id}",
		Summary:     "Rename a sprint or move its dates",
		Description: "Each member is optional and an omitted one is left alone; a date set to null is cleared. The result is checked, so " +
			"moving only `start_date` past the stored `end_date` is a 422 at `body.end_date`. Announced as `sprint.updated`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateSprintInput) (*sprintOutput, error) {
		me, _ := auth.FromContext(ctx)

		s, err := svc.Update(ctx, me.AccountID, in.ID, sprint.Patch{
			Name:      in.Body.Name,
			StartDate: sprint.Nullable{Set: in.Body.StartDate.Set, Value: in.Body.StartDate.Value},
			EndDate:   sprint.Nullable{Set: in.Body.EndDate.Set, Value: in.Body.EndDate.Value},
		})
		if err != nil {
			return nil, sprintError(err)
		}

		feed.publishSprint(s.WorkspaceID, realtime.SprintUpdated, me.AccountID, s)

		return &sprintOutput{Body: asSprintBody(s)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-sprint",
		Method:      http.MethodDelete,
		Path:        V1 + "/sprints/{id}",
		Summary:     "Delete a sprint; its tickets stay",
		Description: "The sprint's memberships go with it. The tickets' rows are not rewritten, so their `version` does not move and no " +
			"`ticket.updated` is sent: `sprint.deleted` is the announcement.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *sprintInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		s, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, sprintError(err)
		}

		feed.publishSprint(s.WorkspaceID, realtime.SprintDeleted, me.AccountID, s)

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "add-ticket-to-sprint",
		Method:      http.MethodPost,
		Path:        V1 + "/sprints/{id}/tickets",
		Summary:     "Put a ticket in a sprint",
		Description: "The ticket must be a live ticket of the sprint's project, or a 422 `ticket_not_found` at `body.ticket_id`. " +
			"A ticket can be in a sprint and an epic at once, and in several sprints. Joining is a change to the ticket: its `version` " +
			"rises by one and it is announced as `ticket.updated`. A ticket that is already in the sprint is a success that writes and " +
			"announces nothing.",
		DefaultStatus: http.StatusNoContent,
		Metadata:      needs(auth.TicketWrite),
	}, func(ctx context.Context, in *sprintTicketInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		t, changed, err := tickets.AddToSprint(ctx, me.AccountID, in.ID, in.Body.TicketID)
		if err != nil {
			return nil, sprintError(err)
		}
		if changed {
			feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)
		}

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "remove-ticket-from-sprint",
		Method:      http.MethodDelete,
		Path:        V1 + "/sprints/{id}/tickets/{ticket_id}",
		Summary:     "Take a ticket out of a sprint",
		Description: "A ticket that is not in the sprint is a success that writes and announces nothing. Leaving is a change to the " +
			"ticket: its `version` rises by one and it is announced as `ticket.updated` (an archived ticket is taken out without either).",
		DefaultStatus: http.StatusNoContent,
		Metadata:      needs(auth.TicketWrite),
	}, func(ctx context.Context, in *sprintTicketPathInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		t, changed, err := tickets.RemoveFromSprint(ctx, me.AccountID, in.ID, in.TicketID)
		if err != nil {
			return nil, sprintError(err)
		}
		if changed {
			feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)
		}

		return nil, nil
	})
}

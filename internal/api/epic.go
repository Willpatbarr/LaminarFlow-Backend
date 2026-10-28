/*
╔═ epic.go ═════════════════════════════════════════════════════════════════════════════
║  http handlers · epics
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeEpicNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  five operations under /epics
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/epic"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

// CodeEpicNotFound is a 404 for an epic id in the path that is absent or out of scope,
// and a 422 at body.epic_id when a ticket write names an epic of another project (or none
// at all) - one code for both, the way status_not_found is.
const CodeEpicNotFound = "epic_not_found"

type epicBody struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description" doc:"Never null; empty when unset."`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type epicOutput struct {
	Body epicBody
}

type createEpicInput struct {
	Body struct {
		ProjectID   string `json:"project_id" required:"true" format:"uuid"`
		Name        string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		Description string `json:"description,omitempty" maxLength:"50000" doc:"Up to 50,000 characters (not bytes). Omit for none."`
	}
}

type listEpicsInput struct {
	Body struct {
		ProjectID string `json:"project_id" required:"true" format:"uuid"`
	}
}

type epicInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateEpicInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name        *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
		Description *string `json:"description,omitempty" maxLength:"50000"`
	}
}

func asEpicBody(e epic.Epic) epicBody {
	return epicBody{ID: e.ID, ProjectID: e.ProjectID, Name: e.Name, Description: e.Description,
		CreatedAt: e.CreatedAt.UTC(), UpdatedAt: e.UpdatedAt.UTC()}
}

func epicError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	switch {
	case errors.Is(err, epic.ErrNotFound):
		return NewError(http.StatusNotFound, CodeEpicNotFound, "no such epic")
	case errors.Is(err, epic.ErrProjectNotFound):
		return NewError(http.StatusNotFound, CodeProjectNotFound, "no such project")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the epic operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *epic.Service    nil only for cmd/openapi
│      feed    ticketFeed       publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/epics/list
*/

func registerEpics(api huma.API, svc *epic.Service, feed ticketFeed) {

	huma.Register(api, huma.Operation{
		OperationID: "create-epic",
		Method:      http.MethodPost,
		Path:        V1 + "/epics",
		Summary:     "Create an epic in a project",
		Description: "Announced as `epic.created`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createEpicInput) (*epicOutput, error) {
		me, _ := auth.FromContext(ctx)

		e, err := svc.Create(ctx, me.AccountID, in.Body.ProjectID, in.Body.Name, in.Body.Description)
		if err != nil {
			return nil, epicError(err)
		}

		feed.publishEpic(e.WorkspaceID, realtime.EpicCreated, me.AccountID, e)

		return &epicOutput{Body: asEpicBody(e)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-epics",
		Method:      http.MethodPost,
		Path:        V1 + "/epics/list",
		Summary:     "List a project's epics (a read - POST for the body)",
		Description: "Oldest first. Always one page: `next_cursor` is null and `has_more` false.",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *listEpicsInput) (*listOutput[epicBody], error) {
		me, _ := auth.FromContext(ctx)

		epics, err := svc.List(ctx, me.AccountID, in.Body.ProjectID)
		if err != nil {
			return nil, epicError(err)
		}

		out := &listOutput[epicBody]{}
		out.Body.Items = make([]epicBody, len(epics))
		for i, e := range epics {
			out.Body.Items[i] = asEpicBody(e)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-epic",
		Method:      http.MethodGet,
		Path:        V1 + "/epics/{id}",
		Summary:     "Fetch one epic",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *epicInput) (*epicOutput, error) {
		me, _ := auth.FromContext(ctx)

		e, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, epicError(err)
		}

		return &epicOutput{Body: asEpicBody(e)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-epic",
		Method:      http.MethodPut,
		Path:        V1 + "/epics/{id}",
		Summary:     "Rename an epic or change its description",
		Description: "Each member is optional and an omitted one is left alone. Announced as `epic.updated`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateEpicInput) (*epicOutput, error) {
		me, _ := auth.FromContext(ctx)

		e, err := svc.Update(ctx, me.AccountID, in.ID, epic.Patch{Name: in.Body.Name, Description: in.Body.Description})
		if err != nil {
			return nil, epicError(err)
		}

		feed.publishEpic(e.WorkspaceID, realtime.EpicUpdated, me.AccountID, e)

		return &epicOutput{Body: asEpicBody(e)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-epic",
		Method:      http.MethodDelete,
		Path:        V1 + "/epics/{id}",
		Summary:     "Delete an epic; its tickets stay and lose the epic",
		Description: "Sets `epic_id` to null on the epic's tickets (SET NULL, not cascade). Those tickets' rows are not rewritten, " +
			"so their `version` does not move and no `ticket.updated` is sent: `epic.deleted` is the announcement, and a client " +
			"clears `epic_id` on the tickets it holds for that epic.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *epicInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		e, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, epicError(err)
		}

		feed.publishEpic(e.WorkspaceID, realtime.EpicDeleted, me.AccountID, e)

		return nil, nil
	})
}

/*
╔═ saved_view.go ═══════════════════════════════════════════════════════════════════════
║  http handlers · saved views
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeSavedViewNotFound    const
║      CodeSavedViewNotOwner    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  five operations under /saved-views
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/savedview"

	"github.com/danielgtaylor/huma/v2"
)

// Two codes. A view the caller cannot see is a 404 - another person's private view is
// indistinguishable from an absent one, as every scope check here is. A shared view the
// caller can see and does not own is a 403, because it is visible and the refusal is
// about who may change it, which a 404 would misreport.
const (
	CodeSavedViewNotFound = "saved_view_not_found"
	CodeSavedViewNotOwner = "saved_view_not_owner"
)

type savedViewBody struct {
	ID             string           `json:"id"`
	TeamID         string           `json:"team_id"`
	ProjectID      *string          `json:"project_id" doc:"Null for a view of the whole team."`
	BoardID        *string          `json:"board_id" doc:"Set exactly when layout is board."`
	OwnerAccountID *string          `json:"owner_account_id" doc:"Null for a team's own view: the one team creation seeds, or a shared view whose owner's account was deleted. Any member who can reach the team may edit or delete such a view."`
	Name           string           `json:"name"`
	Layout         string           `json:"layout" enum:"list,board"`
	Config         savedview.Config `json:"config"`
	IsShared       bool             `json:"is_shared" doc:"false is private to the owner; true is visible to the whole team."`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type savedViewOutput struct {
	Body savedViewBody
}

type createSavedViewInput struct {
	Body struct {
		TeamID    string           `json:"team_id" required:"true" format:"uuid"`
		ProjectID *string          `json:"project_id,omitempty" format:"uuid" doc:"Scope the view to one project of the team. Omit for the whole team."`
		BoardID   *string          `json:"board_id,omitempty" format:"uuid" doc:"Required when layout is board, and refused when it is list. A board of the team, and of the project when one is given."`
		Name      string           `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		Layout    string           `json:"layout" required:"true" enum:"list,board"`
		Config    savedview.Config `json:"config,omitempty" doc:"Omit for an empty config."`
		IsShared  bool             `json:"is_shared,omitempty" doc:"Default false: private to you."`
	}
}

type listSavedViewsInput struct {
	Body struct {
		TeamID    string  `json:"team_id" required:"true" format:"uuid"`
		ProjectID *string `json:"project_id,omitempty" format:"uuid" doc:"With it, that project's views plus the team-wide ones. Omit for every view of the team."`
		Limit     int     `json:"limit,omitempty" minimum:"1" maximum:"200" doc:"Rows per page. Omitted means 50. Over the maximum is rejected, not clamped."`
		Cursor    string  `json:"cursor,omitempty" doc:"next_cursor from the previous page. Opaque - do not construct or parse it."`
	}
}

type savedViewInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateSavedViewInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name     *string           `json:"name,omitempty" minLength:"1" maxLength:"100"`
		Layout   *string           `json:"layout,omitempty" enum:"list,board"`
		Config   *savedview.Config `json:"config,omitempty" doc:"Replaces the whole config; it is not merged."`
		IsShared *bool             `json:"is_shared,omitempty"`
	}
}

func asSavedViewBody(v savedview.View) savedViewBody {
	return savedViewBody{
		ID: v.ID, TeamID: v.TeamID, ProjectID: v.ProjectID, BoardID: v.BoardID,
		OwnerAccountID: v.OwnerAccountID, Name: v.Name, Layout: v.Layout, Config: v.Config,
		IsShared: v.IsShared, CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC(),
	}
}

// savedViewError maps the service's errors. A config the filter language refuses is a
// 422 filter_invalid with one entry per fault, located at body.config... - the same code
// and the same errors[] a list body gets, so one client branch handles both.
func savedViewError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	var fe *filter.Error
	if errors.As(err, &fe) {
		out := NewError(http.StatusUnprocessableEntity, CodeFilterInvalid, "the view's config was not accepted")

		for _, f := range fe.Faults {
			out.Errors = append(out.Errors, &huma.ErrorDetail{Message: f.Message, Location: f.Location, Value: f.Value})
		}

		return out
	}

	unprocessable := defaultCode(http.StatusUnprocessableEntity)

	switch {
	case errors.Is(err, savedview.ErrNotFound):
		return NewError(http.StatusNotFound, CodeSavedViewNotFound, "no such saved view")
	case errors.Is(err, savedview.ErrNotOwner):
		return NewError(http.StatusForbidden, CodeSavedViewNotOwner, "only the owner of a view may change or delete it")
	case errors.Is(err, savedview.ErrTeamNotFound):
		return newFieldError(CodeTeamNotFound, "the team is not one in your workspace", "body.team_id", "no such team")
	case errors.Is(err, savedview.ErrProjectNotFound):
		return newFieldError(CodeProjectNotFound, "the project is not one of this team's", "body.project_id", "no such project in this team")
	case errors.Is(err, savedview.ErrBoardNotFound):
		return newFieldError(CodeBoardNotFound, "the board is not one this view's team and project have", "body.board_id", "no such board for this view")
	case errors.Is(err, savedview.ErrBoardRequired):
		return newFieldError(unprocessable, "a board view needs a board", "body.board_id", "required when layout is board")
	case errors.Is(err, savedview.ErrBoardForbidden):
		return newFieldError(unprocessable, "a list view has no board", "body.board_id", "must be omitted when layout is list")
	case errors.Is(err, savedview.ErrLayoutNeedsBoard):
		return newFieldError(unprocessable, "a list view cannot become a board view", "body.layout",
			"an update cannot name a board - create a board view instead")
	case errors.Is(err, savedview.ErrOwnerless):
		return newFieldError(unprocessable, "a team's own view cannot be made private", "body.is_shared",
			"this view has no owner, so private would hide it from everyone")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the saved view operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *savedview.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/saved-views/list
*/

func registerSavedViews(api huma.API, svc *savedview.Service) {

	huma.Register(api, huma.Operation{
		OperationID: "create-saved-view",
		Method:      http.MethodPost,
		Path:        V1 + "/saved-views",
		Summary:     "Save a list or board setup",
		Description: "Owned by the caller; private unless `is_shared`. `config` holds `filter` (the language of the ticket list), `group`, `sort` and `columns`, " +
			"all over the ticket vocabulary, and is checked the way a ticket list request would be: a config that would not run is a 422 `filter_invalid` " +
			"whose `errors[].location` is `body.config...`. A board view needs `board_id`; a list view must not have one.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createSavedViewInput) (*savedViewOutput, error) {
		me, _ := auth.FromContext(ctx)

		v, err := svc.Create(ctx, me.AccountID, savedview.CreateParams{
			TeamID: in.Body.TeamID, ProjectID: in.Body.ProjectID, BoardID: in.Body.BoardID,
			Name: in.Body.Name, Layout: in.Body.Layout, Config: in.Body.Config, IsShared: in.Body.IsShared,
		})
		if err != nil {
			return nil, savedViewError(err)
		}

		return &savedViewOutput{Body: asSavedViewBody(v)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-saved-views",
		Method:      http.MethodPost,
		Path:        V1 + "/saved-views/list",
		Summary:     "List the views the caller can open (a read - POST for the body)",
		Description: "The caller's own private views plus every shared view of the team, by name. With `project_id`, that project's views and the team-wide ones; " +
			"without it, every view of the team. Keyset pagination as the other lists: pass `next_cursor` back as `cursor`.",
		Metadata: needs(auth.TicketRead),
	}, func(ctx context.Context, in *listSavedViewsInput) (*listOutput[savedViewBody], error) {
		me, _ := auth.FromContext(ctx)

		page, err := svc.List(ctx, me.AccountID, savedview.ListParams{
			TeamID: in.Body.TeamID, ProjectID: in.Body.ProjectID, Limit: in.Body.Limit, Cursor: in.Body.Cursor,
		})
		if err != nil {
			if errors.Is(err, savedview.ErrTeamNotFound) {
				return nil, savedViewError(err)
			}

			return nil, listError(err)
		}

		out := &listOutput[savedViewBody]{Body: listEnvelope[savedViewBody]{
			Items:   make([]savedViewBody, len(page.Views)),
			HasMore: page.HasMore,
		}}
		for i, v := range page.Views {
			out.Body.Items[i] = asSavedViewBody(v)
		}
		if page.NextCursor != "" {
			out.Body.NextCursor = &page.NextCursor
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-saved-view",
		Method:      http.MethodGet,
		Path:        V1 + "/saved-views/{id}",
		Summary:     "Fetch one saved view",
		Description: "`saved_view_not_found` for an absent view, one outside your workspace, and another person's private view alike.",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *savedViewInput) (*savedViewOutput, error) {
		me, _ := auth.FromContext(ctx)

		v, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, savedViewError(err)
		}

		return &savedViewOutput{Body: asSavedViewBody(v)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-saved-view",
		Method:      http.MethodPut,
		Path:        V1 + "/saved-views/{id}",
		Summary:     "Rename a view, change its config, or share or unshare it",
		Description: "Owner only: a shared view the caller does not own is 403 `saved_view_not_owner`. A view with no owner (`owner_account_id` null) is the team's own, " +
			"and any member may edit it, but it cannot be made private. Each member is optional; an omitted one is left alone, and a `config` replaces the old one whole. " +
			"`layout: list` on a board view clears its `board_id`; `layout: board` on a list view is refused, since this body cannot name a board. " +
			"A config that does not compile is 422 `filter_invalid` located at `body.config...`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateSavedViewInput) (*savedViewOutput, error) {
		me, _ := auth.FromContext(ctx)

		v, err := svc.Update(ctx, me.AccountID, in.ID, savedview.Patch{
			Name: in.Body.Name, Layout: in.Body.Layout, Config: in.Body.Config, IsShared: in.Body.IsShared,
		})
		if err != nil {
			return nil, savedViewError(err)
		}

		return &savedViewOutput{Body: asSavedViewBody(v)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-saved-view",
		Method:      http.MethodDelete,
		Path:        V1 + "/saved-views/{id}",
		Summary:     "Delete a saved view",
		Description: "Owner only, with the same exception for a team's own view as update-saved-view. Nothing else is touched: a view holds no tickets.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *savedViewInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.Delete(ctx, me.AccountID, in.ID); err != nil {
			return nil, savedViewError(err)
		}

		return nil, nil
	})
}

/*
╔═ milestone.go ════════════════════════════════════════════════════════════════════════
║  http handlers · milestones
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeMilestoneNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  five operations under /milestones
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/milestone"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

// CodeMilestoneNotFound is a 404 for a milestone id in the path that is absent or out of
// scope, and a 422 at body.milestone_id when a ticket write names a milestone of another
// project (or none at all).
const CodeMilestoneNotFound = "milestone_not_found"

type milestoneBody struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Description string    `json:"description" doc:"Never null; empty when unset."`
	TargetDate  *string   `json:"target_date" format:"date" doc:"YYYY-MM-DD. Null when the milestone has no date. A release is a milestone (LAM-82)."`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type milestoneOutput struct {
	Body milestoneBody
}

type createMilestoneInput struct {
	Body struct {
		ProjectID   string  `json:"project_id" required:"true" format:"uuid"`
		Name        string  `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		Description string  `json:"description,omitempty" maxLength:"50000" doc:"Up to 50,000 characters (not bytes). Omit for none."`
		TargetDate  *string `json:"target_date,omitempty" format:"date" doc:"YYYY-MM-DD. Omit, or null, for none."`
	}
}

type listMilestonesInput struct {
	Body struct {
		ProjectID string `json:"project_id" required:"true" format:"uuid"`
	}
}

type milestoneInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateMilestoneInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name        *string   `json:"name,omitempty" minLength:"1" maxLength:"100"`
		Description *string   `json:"description,omitempty" maxLength:"50000"`
		TargetDate  patchDate `json:"target_date,omitempty" doc:"YYYY-MM-DD. Omit to leave it, null to clear it."`
	}
}

func asMilestoneBody(m milestone.Milestone) milestoneBody {
	return milestoneBody{ID: m.ID, ProjectID: m.ProjectID, Name: m.Name, Description: m.Description,
		TargetDate: m.TargetDate, CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC()}
}

func milestoneError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	switch {
	case errors.Is(err, milestone.ErrNotFound):
		return NewError(http.StatusNotFound, CodeMilestoneNotFound, "no such milestone")
	case errors.Is(err, milestone.ErrProjectNotFound):
		return NewError(http.StatusNotFound, CodeProjectNotFound, "no such project")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the milestone operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *milestone.Service    nil only for cmd/openapi
│      feed    ticketFeed            publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/milestones/list
*/

func registerMilestones(api huma.API, svc *milestone.Service, feed ticketFeed) {

	huma.Register(api, huma.Operation{
		OperationID: "create-milestone",
		Method:      http.MethodPost,
		Path:        V1 + "/milestones",
		Summary:     "Create a milestone in a project",
		Description: "Announced as `milestone.created`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createMilestoneInput) (*milestoneOutput, error) {
		me, _ := auth.FromContext(ctx)

		m, err := svc.Create(ctx, me.AccountID, in.Body.ProjectID, in.Body.Name, in.Body.Description, in.Body.TargetDate)
		if err != nil {
			return nil, milestoneError(err)
		}

		feed.publishMilestone(m.WorkspaceID, realtime.MilestoneCreated, me.AccountID, m)

		return &milestoneOutput{Body: asMilestoneBody(m)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-milestones",
		Method:      http.MethodPost,
		Path:        V1 + "/milestones/list",
		Summary:     "List a project's milestones (a read - POST for the body)",
		Description: "Dated milestones soonest first, then undated ones, oldest first. Always one page: `next_cursor` is null and `has_more` false.",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *listMilestonesInput) (*listOutput[milestoneBody], error) {
		me, _ := auth.FromContext(ctx)

		ms, err := svc.List(ctx, me.AccountID, in.Body.ProjectID)
		if err != nil {
			return nil, milestoneError(err)
		}

		out := &listOutput[milestoneBody]{}
		out.Body.Items = make([]milestoneBody, len(ms))
		for i, m := range ms {
			out.Body.Items[i] = asMilestoneBody(m)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-milestone",
		Method:      http.MethodGet,
		Path:        V1 + "/milestones/{id}",
		Summary:     "Fetch one milestone",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *milestoneInput) (*milestoneOutput, error) {
		me, _ := auth.FromContext(ctx)

		m, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, milestoneError(err)
		}

		return &milestoneOutput{Body: asMilestoneBody(m)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-milestone",
		Method:      http.MethodPut,
		Path:        V1 + "/milestones/{id}",
		Summary:     "Rename a milestone, change its description or move its date",
		Description: "Each member is optional and an omitted one is left alone; `target_date: null` clears the date. Announced as `milestone.updated`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateMilestoneInput) (*milestoneOutput, error) {
		me, _ := auth.FromContext(ctx)

		m, err := svc.Update(ctx, me.AccountID, in.ID, milestone.Patch{
			Name:        in.Body.Name,
			Description: in.Body.Description,
			TargetDate:  milestone.Nullable{Set: in.Body.TargetDate.Set, Value: in.Body.TargetDate.Value},
		})
		if err != nil {
			return nil, milestoneError(err)
		}

		feed.publishMilestone(m.WorkspaceID, realtime.MilestoneUpdated, me.AccountID, m)

		return &milestoneOutput{Body: asMilestoneBody(m)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-milestone",
		Method:      http.MethodDelete,
		Path:        V1 + "/milestones/{id}",
		Summary:     "Delete a milestone; its tickets stay and lose the milestone",
		Description: "Sets `milestone_id` to null on the milestone's tickets (SET NULL, not cascade). Those tickets' rows are not rewritten, " +
			"so their `version` does not move and no `ticket.updated` is sent: `milestone.deleted` is the announcement, and a client " +
			"clears `milestone_id` on the tickets it holds for that milestone.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *milestoneInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		m, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, milestoneError(err)
		}

		feed.publishMilestone(m.WorkspaceID, realtime.MilestoneDeleted, me.AccountID, m)

		return nil, nil
	})
}

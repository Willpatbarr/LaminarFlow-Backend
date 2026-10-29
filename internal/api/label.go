/*
╔═ label.go ════════════════════════════════════════════════════════════════════════════
║  http handlers · labels
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeLabelNotFound     const
║      CodeLabelNameTaken    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  five operations under /labels
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/label"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

const (
	// CodeLabelNotFound is a 404 for a label id in the path that is absent or out of
	// scope, and a 422 at body.label_ids[i] when a ticket write names a label that is not
	// one of its project's team.
	CodeLabelNotFound = "label_not_found"

	// CodeLabelNameTaken is a 409: the team already has a label of that name.
	CodeLabelNameTaken = "label_name_taken"
)

type labelBody struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Name      string    `json:"name" doc:"Unique within the team."`
	Color     string    `json:"color" doc:"Any string up to 32 characters; nothing reads it but the client."`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type labelOutput struct {
	Body labelBody
}

type createLabelInput struct {
	KeyedInput
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
		Name   string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace. Unique within the team, or 409 label_name_taken."`
		Color  string `json:"color" required:"true" minLength:"1" maxLength:"32" doc:"Any string up to 32 characters; nothing reads it but the client."`
	}
}

type listLabelsInput struct {
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
	}
}

type labelInput struct {
	ID string `path:"id" format:"uuid"`
}

type updateLabelInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name  *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
		Color *string `json:"color,omitempty" minLength:"1" maxLength:"32"`
	}
}

func asLabelBody(l label.Label) labelBody {
	return labelBody{ID: l.ID, TeamID: l.TeamID, Name: l.Name, Color: l.Color,
		CreatedAt: l.CreatedAt.UTC(), UpdatedAt: l.UpdatedAt.UTC()}
}

func labelError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	switch {
	case errors.Is(err, label.ErrNotFound):
		return NewError(http.StatusNotFound, CodeLabelNotFound, "no such label")
	case errors.Is(err, label.ErrTeamNotFound):
		return NewError(http.StatusNotFound, CodeTeamNotFound, "no such team")
	case errors.Is(err, label.ErrNameTaken):
		return NewError(http.StatusConflict, CodeLabelNameTaken, "a label of that name already exists in this team")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the label operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *label.Service    nil only for cmd/openapi
│      feed    ticketFeed        publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/labels/list
*/

func registerLabels(api huma.API, svc *label.Service, feed ticketFeed, idem *idempotency.Store) {

	huma.Register(api, huma.Operation{
		OperationID: "create-label",
		Method:      http.MethodPost,
		Path:        V1 + "/labels",
		Summary:     "Create a label in a team",
		Description: "A name already used in the team is a 409 `label_name_taken`. Announced as `label.created`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createLabelInput) (*createdOutput[labelBody], error) {
		me, _ := auth.FromContext(ctx)

		l, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "create-label", label.ErrNotFound,
			func(ctx context.Context) (label.Label, string, error) {
				l, err := svc.Create(ctx, me.AccountID, in.Body.TeamID, in.Body.Name, in.Body.Color)
				return l, l.ID, err
			},
			func(ctx context.Context, id string) (label.Label, error) { return svc.Get(ctx, me.AccountID, id) })
		if err != nil {
			return nil, labelError(err)
		}

		if !replayed {
			feed.publishLabel(l.WorkspaceID, realtime.LabelCreated, me.AccountID, l)
		}

		return created(in.key(), replayed, asLabelBody(l)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-labels",
		Method:      http.MethodPost,
		Path:        V1 + "/labels/list",
		Summary:     "List a team's labels (a read - POST for the body)",
		Description: "By name. Always one page: `next_cursor` is null and `has_more` false.",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *listLabelsInput) (*listOutput[labelBody], error) {
		me, _ := auth.FromContext(ctx)

		labels, err := svc.List(ctx, me.AccountID, in.Body.TeamID)
		if err != nil {
			return nil, labelError(err)
		}

		out := &listOutput[labelBody]{}
		out.Body.Items = make([]labelBody, len(labels))
		for i, l := range labels {
			out.Body.Items[i] = asLabelBody(l)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-label",
		Method:      http.MethodGet,
		Path:        V1 + "/labels/{id}",
		Summary:     "Fetch one label",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *labelInput) (*labelOutput, error) {
		me, _ := auth.FromContext(ctx)

		l, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, labelError(err)
		}

		return &labelOutput{Body: asLabelBody(l)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-label",
		Method:      http.MethodPut,
		Path:        V1 + "/labels/{id}",
		Summary:     "Rename or recolour a label",
		Description: "Each member is optional and an omitted one is left alone. A name already used in the team is a 409 `label_name_taken`. " +
			"The id never changes, so tickets keep the label. Announced as `label.updated`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateLabelInput) (*labelOutput, error) {
		me, _ := auth.FromContext(ctx)

		l, err := svc.Update(ctx, me.AccountID, in.ID, label.Patch{Name: in.Body.Name, Color: in.Body.Color})
		if err != nil {
			return nil, labelError(err)
		}

		feed.publishLabel(l.WorkspaceID, realtime.LabelUpdated, me.AccountID, l)

		return &labelOutput{Body: asLabelBody(l)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-label",
		Method:      http.MethodDelete,
		Path:        V1 + "/labels/{id}",
		Summary:     "Delete a label; its tickets stay and lose the label",
		Description: "Removes the label from every ticket that carried it (cascade). Those tickets' rows are not rewritten, so their " +
			"`version` does not move and no `ticket.updated` is sent: `label.deleted` is the announcement, and a client drops the id from " +
			"`label_ids` on the tickets it holds.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *labelInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		l, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, labelError(err)
		}

		feed.publishLabel(l.WorkspaceID, realtime.LabelDeleted, me.AccountID, l)

		return nil, nil
	})
}

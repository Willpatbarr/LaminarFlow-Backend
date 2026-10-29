/*
╔═ status.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · editing a team's statuses
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeStatusInUse       const
║      CodeStatusSetWrong    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  four operations under /teams/{id}/statuses
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"

	"github.com/danielgtaylor/huma/v2"
)

const (
	// CodeStatusNotFound (ticket.go) is also used here: a 404 for an unknown or foreign
	// status in the path, and a 422 at query.move_to for a move_to that is not a status
	// of this team.

	// CodeStatusInUse is a delete refused because it would strand tickets.
	CodeStatusInUse = "status_in_use"

	// CodeStatusSetWrong is a reorder list that is partial, duplicated or foreign. A 422
	// rather than aspect's 409: LAM-83 specifies body.status_ids.
	CodeStatusSetWrong = "status_set_mismatch"
)

type statusOutput struct {
	Body statusBody
}

type createStatusInput struct {
	KeyedInput
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name     string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters, and not only whitespace."`
		Color    string `json:"color" required:"true" minLength:"1" maxLength:"32" doc:"Any string up to 32 characters; nothing reads it but the client."`
		Category string `json:"category" required:"true" enum:"not_started,in_progress,done"`
		Position *int   `json:"position,omitempty" doc:"An ordering hint; duplicates are legal. Omitted means last."`
	}
}

type updateStatusInput struct {
	ID       string `path:"id" format:"uuid"`
	StatusID string `path:"status_id" format:"uuid"`
	Body     struct {
		Name     *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
		Color    *string `json:"color,omitempty" minLength:"1" maxLength:"32"`
		Category *string `json:"category,omitempty" enum:"not_started,in_progress,done"`
	}
}

type deleteStatusInput struct {
	ID       string `path:"id" format:"uuid"`
	StatusID string `path:"status_id" format:"uuid"`
	MoveTo   string `query:"move_to" format:"uuid" doc:"Another status of this team to move the tickets to. Required when tickets use this one."`
}

type reorderStatusesInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		StatusIDs []string `json:"status_ids" required:"true" doc:"Every status of this team, exactly once, in the order wanted."`
	}
}

// statusError maps the service's failures. A textlimit refusal is already a 422 at
// body.<field> through limitError.
func statusError(err error) error {
	if e := limitError(err); e != nil {
		return e
	}

	var inUse *team.StatusInUse
	switch {
	case errors.Is(err, team.ErrNotFound):
		return NewError(http.StatusNotFound, CodeTeamNotFound, "no such team")

	case errors.Is(err, team.ErrStatusNotFound):
		return NewError(http.StatusNotFound, CodeStatusNotFound, "no such status")

	case errors.Is(err, team.ErrMoveToNotFound):
		return newFieldError(CodeStatusNotFound, "move_to is not another status of this team",
			"query.move_to", "must be a different status of this team")

	case errors.Is(err, team.ErrStatusSetWrong):
		return newFieldError(CodeStatusSetWrong, "the order must name every status of this team, exactly once",
			"body.status_ids", "must list every status of this team exactly once")

	case errors.As(err, &inUse):
		if inUse.Last {
			return newFieldError(CodeStatusInUse,
				fmt.Sprintf("this is the team's only status and %s; none could take them", plural(inUse.Count, "ticket exists", "tickets exist")),
				"query.move_to", "no other status to move to")
		}

		return newFieldError(CodeStatusInUse,
			fmt.Sprintf("%s; pass move_to", plural(inUse.Count, "ticket uses this status", "tickets use this status")),
			"query.move_to", "required while tickets use this status")
	}

	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}

	return fmt.Sprintf("%d %s", n, many)
}

/*
┌─ api ───────────────────────────────────────────
│  registers the status editor's operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *team.Service    nil only for cmd/openapi
│      feed    ticketFeed       publishes each committed write
├─ example ───────────────────────────────────────
│      →  DELETE /api/v1/teams/{id}/statuses/{status_id}?move_to=…
*/

func registerStatuses(api huma.API, svc *team.Service, feed ticketFeed, idem *idempotency.Store) {

	huma.Register(api, huma.Operation{
		OperationID: "create-status",
		Method:      http.MethodPost,
		Path:        V1 + "/teams/{id}/statuses",
		Summary:     "Add a status to a team",
		Description: "No status is sacred, so there is no special case and no 409: name and position are not unique.",
		Metadata:    needs(auth.TeamWrite),
	}, func(ctx context.Context, in *createStatusInput) (*createdOutput[statusBody], error) {
		me, _ := auth.FromContext(ctx)

		var ws string
		st, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "create-status", team.ErrStatusNotFound,
			func(ctx context.Context) (team.Status, string, error) {
				st, w, err := svc.CreateStatus(ctx, me.AccountID, in.ID, team.StatusParams{
					Name: in.Body.Name, Color: in.Body.Color, Category: in.Body.Category, Position: in.Body.Position,
				})
				ws = w
				return st, st.ID, err
			},
			func(ctx context.Context, id string) (team.Status, error) {
				// The status's team is the one in the path; a status that moved or went is gone.
				t, err := svc.Get(ctx, me.AccountID, in.ID)
				if err != nil {
					if errors.Is(err, team.ErrNotFound) {
						return team.Status{}, team.ErrStatusNotFound
					}
					return team.Status{}, err
				}
				for _, s := range t.Statuses {
					if s.ID == id {
						return s, nil
					}
				}
				return team.Status{}, team.ErrStatusNotFound
			})
		if err != nil {
			return nil, statusError(err)
		}

		if !replayed {
			feed.publishStatus(ws, realtime.StatusCreated, me.AccountID, in.ID, st)
		}

		return created(in.key(), replayed, asStatusBody(st)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-status",
		Method:      http.MethodPut,
		Path:        V1 + "/teams/{id}/statuses/{status_id}",
		Summary:     "Rename, recolour or recategorise a status",
		Description: "Each member is optional and an omitted one is left alone. The id never changes, so tickets keep their status.",
		Metadata:    needs(auth.TeamWrite),
	}, func(ctx context.Context, in *updateStatusInput) (*statusOutput, error) {
		me, _ := auth.FromContext(ctx)

		st, ws, err := svc.UpdateStatus(ctx, me.AccountID, in.ID, in.StatusID, team.StatusPatch{
			Name: in.Body.Name, Color: in.Body.Color, Category: in.Body.Category,
		})
		if err != nil {
			return nil, statusError(err)
		}

		feed.publishStatus(ws, realtime.StatusUpdated, me.AccountID, in.ID, st)

		return &statusOutput{Body: asStatusBody(st)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-status",
		Method:      http.MethodDelete,
		Path:        V1 + "/teams/{id}/statuses/{status_id}",
		Summary:     "Delete a status, moving its tickets",
		Description: "Tickets that use the status need a `move_to` status, or the answer is a 422 `status_in_use` at `query.move_to` " +
			"naming how many. Deleting a team's last status is allowed only while the team has no tickets. " +
			"Tickets that move are written like any edit (version + 1) and each is announced as `ticket.updated`. " +
			"Board columns mapped to the deleted status are mapped to `move_to` instead when tickets move, and unmapped otherwise.",
		Metadata: needs(auth.TeamWrite),
	}, func(ctx context.Context, in *deleteStatusInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		var moveTo *string
		if in.MoveTo != "" {
			moveTo = &in.MoveTo
		}

		res, err := svc.DeleteStatus(ctx, me.AccountID, in.ID, in.StatusID, moveTo)
		if err != nil {
			return nil, statusError(err)
		}

		feed.publishStatus(res.WorkspaceID, realtime.StatusDeleted, me.AccountID, in.ID, res.Status)
		feed.publishMoved(res.WorkspaceID, me.AccountID, res.Moved)

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reorder-statuses",
		Method:      http.MethodPost,
		Path:        V1 + "/teams/{id}/statuses/reorder",
		Summary:     "Set the order of every status",
		Description: "Send the complete list, in the order wanted; positions become 1..n. A partial, duplicated or foreign list is a 422 at body.status_ids. " +
			"Each status whose position changed is announced as `status.updated`.",
		Metadata: needs(auth.TeamWrite),
	}, func(ctx context.Context, in *reorderStatusesInput) (*teamOutput, error) {
		me, _ := auth.FromContext(ctx)

		changed, ws, err := svc.ReorderStatuses(ctx, me.AccountID, in.ID, in.Body.StatusIDs)
		if err != nil {
			return nil, statusError(err)
		}
		for _, st := range changed {
			feed.publishStatus(ws, realtime.StatusUpdated, me.AccountID, in.ID, st)
		}

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, teamError(err)
		}

		return &teamOutput{Body: asTeamBody(t)}, nil
	})
}

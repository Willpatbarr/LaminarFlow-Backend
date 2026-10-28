/*
╔═ board.go ════════════════════════════════════════════════════════════════════════════
║  http handlers · boards, their columns, and the statuses each column holds
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeBoardNotFound         const
║      CodeColumnNotFound        const
║      CodeStatusAlreadyMapped   const
║      CodeColumnSetWrong        const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  ten operations under /boards and /projects/{id}/boards
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/board"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

const (
	// CodeBoardNotFound and CodeColumnNotFound are 404s for an id in the path that is
	// absent, out of scope, or (for a column) on another board.
	CodeBoardNotFound  = "board_not_found"
	CodeColumnNotFound = "column_not_found"

	// CodeStatusAlreadyMapped is a status that another column of the same board holds.
	// A status sits in one column of a board; the schema allows more and the service does
	// not. (CodeStatusNotFound, in ticket.go, is the other 422 on body.status_ids.)
	CodeStatusAlreadyMapped = "status_already_mapped"

	// CodeColumnSetWrong is a column reorder that is partial, duplicated or foreign. A
	// 422 at body.column_ids, like the status reorder rather than the aspect one.
	CodeColumnSetWrong = "column_set_mismatch"
)

type columnBody struct {
	ID        string   `json:"id"`
	BoardID   string   `json:"board_id"`
	Name      string   `json:"name" doc:"The column's own label. Not a status name: one column may hold several statuses."`
	Position  int      `json:"position" doc:"An ordering hint, not a slot. Duplicates are legal; ties read in id order."`
	IsVisible bool     `json:"is_visible" doc:"Whether the board shows the column. Stored per board, because a column belongs to one board."`
	StatusIDs []string `json:"status_ids" doc:"The statuses whose tickets this column shows. A status is in at most one column of a board."`
}

type boardBody struct {
	ID        string       `json:"id"`
	ProjectID string       `json:"project_id"`
	Name      string       `json:"name"`
	GroupBy   string       `json:"group_by" enum:"status" doc:"How tickets are grouped into columns. status is the only value."`
	Columns   []columnBody `json:"columns" doc:"In position order."`
}

type boardOutput struct {
	Body boardBody
}

type columnOutput struct {
	Body columnBody
}

type createBoardInput struct {
	ID   string `path:"id" format:"uuid" doc:"The project."`
	Body struct {
		Name    string `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace."`
		GroupBy string `json:"group_by,omitempty" enum:"status" default:"status" doc:"Only status is legal. Optional."`
	}
}

type listBoardsInput struct {
	Body struct {
		ProjectID string `json:"project_id" required:"true" format:"uuid"`
	}
}

type boardInput struct {
	ID string `path:"id" format:"uuid"`
}

type renameBoardInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name string `json:"name" required:"true" minLength:"1" maxLength:"100"`
	}
}

type addColumnInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name      string   `json:"name" required:"true" minLength:"1" maxLength:"100"`
		StatusIDs []string `json:"status_ids,omitempty" doc:"Statuses of the board's team for the new column to hold. Optional; none by default."`
	}
}

type columnInput struct {
	ID       string `path:"id" format:"uuid"`
	ColumnID string `path:"column_id" format:"uuid"`
}

type updateColumnInput struct {
	ID       string `path:"id" format:"uuid"`
	ColumnID string `path:"column_id" format:"uuid"`
	Body     struct {
		Name      *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
		IsVisible *bool   `json:"is_visible,omitempty"`
	}
}

type setColumnStatusesInput struct {
	ID       string `path:"id" format:"uuid"`
	ColumnID string `path:"column_id" format:"uuid"`
	Body     struct {
		StatusIDs []string `json:"status_ids" required:"true" doc:"The full set this column holds. Empty unmaps every status."`
	}
}

type reorderColumnsInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		ColumnIDs []string `json:"column_ids" required:"true" doc:"Every column of this board, exactly once, in the order wanted."`
	}
}

func asColumnBody(c board.Column) columnBody {
	out := columnBody{ID: c.ID, BoardID: c.BoardID, Name: c.Name, Position: c.Position,
		IsVisible: c.IsVisible, StatusIDs: c.StatusIDs}
	if out.StatusIDs == nil {
		out.StatusIDs = []string{}
	}

	return out
}

func asBoardBody(b board.Board) boardBody {
	out := boardBody{ID: b.ID, ProjectID: b.ProjectID, Name: b.Name, GroupBy: b.GroupBy,
		Columns: make([]columnBody, len(b.Columns))}
	for i, c := range b.Columns {
		out.Columns[i] = asColumnBody(c)
	}

	return out
}

// boardError maps the service's failures. indexed puts a status refusal at
// body.status_ids[i] rather than body.status_ids: add-column is specified with the index
// and set-column-statuses without.
func boardError(err error, indexed bool) error {
	if e := limitError(err); e != nil {
		return e
	}

	var se *board.StatusError
	switch {
	case errors.Is(err, board.ErrNotFound):
		return NewError(http.StatusNotFound, CodeBoardNotFound, "no such board")

	case errors.Is(err, board.ErrColumnNotFound):
		return NewError(http.StatusNotFound, CodeColumnNotFound, "no such column on this board")

	case errors.Is(err, board.ErrProjectNotFound):
		return NewError(http.StatusNotFound, CodeProjectNotFound, "no such project")

	case errors.Is(err, board.ErrColumnSetWrong):
		return newFieldError(CodeColumnSetWrong, "the order must name every column of this board, exactly once",
			"body.column_ids", "must list every column of this board exactly once")

	case errors.As(err, &se):
		loc := "body.status_ids"
		if indexed {
			loc = fmt.Sprintf("body.status_ids[%d]", se.Index)
		}

		if errors.Is(se.Kind, board.ErrStatusMapped) {
			return newFieldError(CodeStatusAlreadyMapped, "that status is already on another column of this board",
				loc, "already mapped to another column of this board")
		}

		return newFieldError(CodeStatusNotFound, "the status is not one this board's team has",
			loc, "no such status for this board's team")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the board operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *board.Service    nil only for cmd/openapi
│      feed    ticketFeed        publishes each committed write
├─ example ───────────────────────────────────────
│      →  PUT /api/v1/boards/{id}/columns/{column_id}/statuses
*/

func registerBoards(api huma.API, svc *board.Service, feed ticketFeed) {
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-board",
		Method:      http.MethodPost,
		Path:        V1 + "/projects/{id}/boards",
		Summary:     "Create a board on a project",
		Description: "Starts with one column per status the team has, each mapped to its status, like the board a project is created with. " +
			"Many boards per project; none is privileged. Announced as `board.created`.",
		Metadata: requires,
	}, func(ctx context.Context, in *createBoardInput) (*boardOutput, error) {
		me, _ := auth.FromContext(ctx)

		b, err := svc.Create(ctx, me.AccountID, in.ID, in.Body.Name, in.Body.GroupBy)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishBoard(b.WorkspaceID, realtime.BoardCreated, me.AccountID, b)

		return &boardOutput{Body: asBoardBody(b)}, nil
	})

	registerBoardList(api, svc)

	huma.Register(api, huma.Operation{
		OperationID: "get-board",
		Method:      http.MethodGet,
		Path:        V1 + "/boards/{id}",
		Summary:     "Fetch one board with its columns",
		Metadata:    requires,
	}, func(ctx context.Context, in *boardInput) (*boardOutput, error) {
		me, _ := auth.FromContext(ctx)

		b, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, boardError(err, false)
		}

		return &boardOutput{Body: asBoardBody(b)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-board",
		Method:      http.MethodPut,
		Path:        V1 + "/boards/{id}",
		Summary:     "Rename a board",
		Description: "Announced as `board.updated`.",
		Metadata:    requires,
	}, func(ctx context.Context, in *renameBoardInput) (*boardOutput, error) {
		me, _ := auth.FromContext(ctx)

		b, err := svc.Rename(ctx, me.AccountID, in.ID, in.Body.Name)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishBoard(b.WorkspaceID, realtime.BoardUpdated, me.AccountID, b)

		return &boardOutput{Body: asBoardBody(b)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-board",
		Method:      http.MethodDelete,
		Path:        V1 + "/boards/{id}",
		Summary:     "Delete a board",
		Description: "Its columns, their mappings and the saved views that point at it go with it. Tickets are not touched. " +
			"A project's last board may be deleted; a project with no board is legal. Announced as `board.deleted`.",
		Metadata: requires,
	}, func(ctx context.Context, in *boardInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		b, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishBoard(b.WorkspaceID, realtime.BoardDeleted, me.AccountID, b)

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "add-column",
		Method:      http.MethodPost,
		Path:        V1 + "/boards/{id}/columns",
		Summary:     "Add a column to a board",
		Description: "Appended after the last column. `status_ids` is optional; a status outside the board's team is a 422 `status_not_found` and one " +
			"another column of this board holds is a 422 `status_already_mapped`, both at `body.status_ids[i]`. Announced as `board.column.updated`.",
		Metadata: requires,
	}, func(ctx context.Context, in *addColumnInput) (*columnOutput, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.AddColumn(ctx, me.AccountID, in.ID, in.Body.Name, in.Body.StatusIDs)
		if err != nil {
			return nil, boardError(err, true)
		}

		feed.publishColumn(res.Board.WorkspaceID, me.AccountID, res)

		return &columnOutput{Body: asColumnBody(res.Column)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-column",
		Method:      http.MethodPut,
		Path:        V1 + "/boards/{id}/columns/{column_id}",
		Summary:     "Rename a column or show or hide it",
		Description: "Each member is optional and an omitted one is left alone. `is_visible` is stored on the column, which belongs to one board, so hiding it affects that board only. Announced as `board.column.updated`.",
		Metadata:    requires,
	}, func(ctx context.Context, in *updateColumnInput) (*columnOutput, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.UpdateColumn(ctx, me.AccountID, in.ID, in.ColumnID, in.Body.Name, in.Body.IsVisible)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishColumn(res.Board.WorkspaceID, me.AccountID, res)

		return &columnOutput{Body: asColumnBody(res.Column)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-column-statuses",
		Method:      http.MethodPut,
		Path:        V1 + "/boards/{id}/columns/{column_id}/statuses",
		Summary:     "Set the statuses a column holds",
		Description: "The full set for this column; an empty list unmaps every status. A status outside the board's team is a 422 `status_not_found`; " +
			"one another column of this board holds is a 422 `status_already_mapped`, both at `body.status_ids`. Announced as `board.column.updated`.",
		Metadata: requires,
	}, func(ctx context.Context, in *setColumnStatusesInput) (*columnOutput, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.SetColumnStatuses(ctx, me.AccountID, in.ID, in.ColumnID, in.Body.StatusIDs)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishColumn(res.Board.WorkspaceID, me.AccountID, res)

		return &columnOutput{Body: asColumnBody(res.Column)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-column",
		Method:      http.MethodDelete,
		Path:        V1 + "/boards/{id}/columns/{column_id}",
		Summary:     "Delete a column",
		Description: "Its statuses are unmapped. Tickets are not touched. Announced as `board.updated`, carrying the board without the column.",
		Metadata:    requires,
	}, func(ctx context.Context, in *columnInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.DeleteColumn(ctx, me.AccountID, in.ID, in.ColumnID)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishBoard(res.Board.WorkspaceID, realtime.BoardUpdated, me.AccountID, res.Board)

		return nil, nil
	})

	// POST on /columns/reorder, as the status and aspect reorders do, so "reorder" never
	// sits where a column id goes.
	huma.Register(api, huma.Operation{
		OperationID: "reorder-columns",
		Method:      http.MethodPost,
		Path:        V1 + "/boards/{id}/columns/reorder",
		Summary:     "Set the order of every column",
		Description: "Send the complete list, in the order wanted; positions become 1..n. A partial, duplicated or foreign list is a 422 `column_set_mismatch` at `body.column_ids`. Announced as `board.updated`.",
		Metadata:    requires,
	}, func(ctx context.Context, in *reorderColumnsInput) (*boardOutput, error) {
		me, _ := auth.FromContext(ctx)

		b, err := svc.ReorderColumns(ctx, me.AccountID, in.ID, in.Body.ColumnIDs)
		if err != nil {
			return nil, boardError(err, false)
		}

		feed.publishBoard(b.WorkspaceID, realtime.BoardUpdated, me.AccountID, b)

		return &boardOutput{Body: asBoardBody(b)}, nil
	})
}

// registerBoardList is a POST for the reason every list is (a body, not a query string),
// in the shared envelope, but takes a project rather than a filter: a project has a
// handful of boards, so there is no vocabulary to filter or sort and never a second page.
func registerBoardList(api huma.API, svc *board.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-boards",
		Method:      http.MethodPost,
		Path:        V1 + "/boards/list",
		Summary:     "List a project's boards, columns included (a read - POST for the body)",
		Description: "Oldest first. Always one page: `next_cursor` is null and `has_more` false.",
		Metadata:    map[string]any{RequireAuth: true},
	}, func(ctx context.Context, in *listBoardsInput) (*listOutput[boardBody], error) {
		me, _ := auth.FromContext(ctx)

		boards, err := svc.List(ctx, me.AccountID, in.Body.ProjectID)
		if err != nil {
			return nil, boardError(err, false)
		}

		out := &listOutput[boardBody]{}
		out.Body.Items = make([]boardBody, len(boards))
		for i, b := range boards {
			out.Body.Items[i] = asBoardBody(b)
		}

		return out, nil
	})
}

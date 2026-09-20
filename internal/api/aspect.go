/*
╔═ aspect.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · the aspect type editor
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeAspectNotFound      const
║      CodeFieldSetWrong       const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  eight operations under /aspect-types
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"

	"github.com/danielgtaylor/huma/v2"
)

// One 404 for a type and for a field alike: both collapse absent and out-of-scope, and
// a code that distinguished them would undo the collapse.
const CodeAspectNotFound = "aspect_type_not_found"

// A reorder that names the wrong set is its own failure. It is not a validation error
// the schema could have caught - the set is only wrong relative to the rows that exist
// - so it needs a code a client can branch on to refetch and retry.
const CodeFieldSetWrong = "field_set_mismatch"

// Eight operations rather than one PUT over a whole aspect type, and that is the shape
// LAM-44 is about. A PUT of the full type would make every field edit a body rewrite
// at the API too, and two editors saving different fields would clobber each other -
// which is the failure the table exists to prevent, reintroduced one layer up.

type fieldBody struct {
	ID       string `json:"id" doc:"Stable forever. document.body is keyed by this, so it never changes - not even when the label does."`
	Label    string `json:"label"`
	Position int    `json:"position" doc:"An ordering hint, not a slot. Duplicates are legal."`
}

type aspectTypeBody struct {
	ID     string      `json:"id"`
	TeamID string      `json:"team_id"`
	Name   string      `json:"name"`
	Fields []fieldBody `json:"fields" doc:"In position order. Empty on the list endpoint, which does not load them."`
}

type aspectTypeOutput struct {
	Body aspectTypeBody
}

type fieldOutput struct {
	Body fieldBody
}

type aspectTypeListOutput struct {
	Body struct {
		Items []aspectTypeBody `json:"items"`
	}
}

type createAspectTypeInput struct {
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
		Name   string `json:"name" required:"true" minLength:"1"`
	}
}

type listAspectTypesInput struct {
	TeamID string `query:"team_id" required:"true" format:"uuid"`
}

type aspectTypeInput struct {
	ID string `path:"id" format:"uuid"`
}

type renameAspectTypeInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name string `json:"name" required:"true" minLength:"1"`
	}
}

type addFieldInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Label string `json:"label" required:"true" minLength:"1"`
	}
}

type fieldInput struct {
	ID      string `path:"id" format:"uuid"`
	FieldID string `path:"field_id" format:"uuid"`
}

type renameFieldInput struct {
	ID      string `path:"id" format:"uuid"`
	FieldID string `path:"field_id" format:"uuid"`
	Body    struct {
		Label string `json:"label" required:"true" minLength:"1"`
	}
}

type reorderInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		FieldIDs []string `json:"field_ids" required:"true" minItems:"1" doc:"Every field of this type, exactly once, in the order wanted."`
	}
}

func asFieldBody(f aspect.Field) fieldBody {
	return fieldBody{ID: f.ID, Label: f.Label, Position: f.Position}
}

func asAspectTypeBody(t aspect.Type) aspectTypeBody {
	out := aspectTypeBody{
		ID:     t.ID,
		TeamID: t.TeamID,
		Name:   t.Name,
		Fields: make([]fieldBody, len(t.Fields)),
	}
	for i, f := range t.Fields {
		out.Fields[i] = asFieldBody(f)
	}

	return out
}

// aspectError maps the service's two errors. Anything else is a fault on this side and
// must not be reported as a 404.
func aspectError(err error) error {
	switch {
	case errors.Is(err, aspect.ErrNotFound):
		return NewError(http.StatusNotFound, CodeAspectNotFound, "no such aspect type or field")

	case errors.Is(err, aspect.ErrFieldSetWrong):
		// 409 rather than 422: the request is well formed and would have been
		// right a moment ago. Someone else added or removed a field, so the
		// client refetches and sends the order again.
		return NewError(http.StatusConflict, CodeFieldSetWrong,
			"the order must name every field of this type, exactly once - refetch and retry")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers the aspect type editor's operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *aspect.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/aspect-types/{id}/fields
*/

func registerAspectTypes(api huma.API, svc *aspect.Service) {
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-aspect-type",
		Method:      http.MethodPost,
		Path:        V1 + "/aspect-types",
		Summary:     "Create an aspect type",
		Description: "Created with no fields. An aspect type with none is legal - it renders as a document with a title and nothing else.",
		Metadata:    requires,
	}, func(ctx context.Context, in *createAspectTypeInput) (*aspectTypeOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.CreateType(ctx, me.AccountID, in.Body.TeamID, in.Body.Name)
		if err != nil {
			return nil, aspectError(err)
		}

		return &aspectTypeOutput{Body: asAspectTypeBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-aspect-types",
		Method:      http.MethodGet,
		Path:        V1 + "/aspect-types",
		Summary:     "List a team's aspect types",
		Description: "Names only - fields are empty here. Fetch one type to get its fields.",
		Metadata:    requires,
	}, func(ctx context.Context, in *listAspectTypesInput) (*aspectTypeListOutput, error) {
		me, _ := auth.FromContext(ctx)

		types, err := svc.ListTypes(ctx, me.AccountID, in.TeamID)
		if err != nil {
			return nil, aspectError(err)
		}

		out := &aspectTypeListOutput{}
		out.Body.Items = make([]aspectTypeBody, len(types))
		for i, t := range types {
			out.Body.Items[i] = asAspectTypeBody(t)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-aspect-type",
		Method:      http.MethodGet,
		Path:        V1 + "/aspect-types/{id}",
		Summary:     "Fetch one aspect type with its fields",
		Metadata:    requires,
	}, func(ctx context.Context, in *aspectTypeInput) (*aspectTypeOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, aspectError(err)
		}

		return &aspectTypeOutput{Body: asAspectTypeBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-aspect-type",
		Method:      http.MethodPut,
		Path:        V1 + "/aspect-types/{id}",
		Summary:     "Rename an aspect type",
		Metadata:    requires,
	}, func(ctx context.Context, in *renameAspectTypeInput) (*aspectTypeOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.RenameType(ctx, me.AccountID, in.ID, in.Body.Name)
		if err != nil {
			return nil, aspectError(err)
		}

		return &aspectTypeOutput{Body: asAspectTypeBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "add-aspect-type-field",
		Method:      http.MethodPost,
		Path:        V1 + "/aspect-types/{id}/fields",
		Summary:     "Add a field to an aspect type",
		Description: "An insert, and nothing else. No document is touched - the new field is simply a key absent from every body, which renders empty.",
		Metadata:    requires,
	}, func(ctx context.Context, in *addFieldInput) (*fieldOutput, error) {
		me, _ := auth.FromContext(ctx)

		f, err := svc.AddField(ctx, me.AccountID, in.ID, in.Body.Label)
		if err != nil {
			return nil, aspectError(err)
		}

		return &fieldOutput{Body: asFieldBody(f)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-aspect-type-field",
		Method:      http.MethodPut,
		Path:        V1 + "/aspect-types/{id}/fields/{field_id}",
		Summary:     "Rename a field",
		Description: "The field's id does not change, so every document keeps the values stored under it.",
		Metadata:    requires,
	}, func(ctx context.Context, in *renameFieldInput) (*fieldOutput, error) {
		me, _ := auth.FromContext(ctx)

		f, err := svc.RenameField(ctx, me.AccountID, in.FieldID, in.Body.Label)
		if err != nil {
			return nil, aspectError(err)
		}

		return &fieldOutput{Body: asFieldBody(f)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "remove-aspect-type-field",
		Method:      http.MethodDelete,
		Path:        V1 + "/aspect-types/{id}/fields/{field_id}",
		Summary:     "Remove a field",
		Description: "Deletes the field row. Values already stored under its id stay in every document's body and simply stop being rendered - " +
			"stripping them would be unrecoverable, and would mean rewriting every body of this type.",
		Metadata: requires,
	}, func(ctx context.Context, in *fieldInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.RemoveField(ctx, me.AccountID, in.FieldID); err != nil {
			return nil, aspectError(err)
		}

		return nil, nil
	})

	// POST rather than PUT on a /fields/order path: "order" would sit where a field
	// id goes, and a route that is ambiguous with its neighbour is a route someone
	// eventually reaches by accident.
	huma.Register(api, huma.Operation{
		OperationID: "reorder-aspect-type-fields",
		Method:      http.MethodPost,
		Path:        V1 + "/aspect-types/{id}/reorder",
		Summary:     "Set the order of every field",
		Description: "Send the complete list, in the order wanted. Idempotent, and the result is exactly what was sent - " +
			"a partial list is rejected, because it would renumber some fields and leave others.",
		Metadata: requires,
	}, func(ctx context.Context, in *reorderInput) (*aspectTypeOutput, error) {
		me, _ := auth.FromContext(ctx)

		fields, err := svc.Reorder(ctx, me.AccountID, in.ID, in.Body.FieldIDs)
		if err != nil {
			return nil, aspectError(err)
		}

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, aspectError(err)
		}
		t.Fields = fields

		return &aspectTypeOutput{Body: asAspectTypeBody(t)}, nil
	})
}

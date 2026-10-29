/*
╔═ aspect.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · the aspect type editor
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeAspectNotFound      const
║      CodeFieldSetWrong       const
║      CodeAspectFormatUnsupported   const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  eleven operations under /aspect-types
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"

	"github.com/danielgtaylor/huma/v2"
)

// One 404 for a type and for a field alike: both collapse absent and out-of-scope, and
// a code that distinguished them would undo the collapse.
const CodeAspectNotFound = "aspect_type_not_found"

// A reorder that names the wrong set is its own failure. It is not a validation error
// the schema could have caught - the set is only wrong relative to the rows that exist
// - so it needs a code a client can branch on to refetch and retry.
const CodeFieldSetWrong = "field_set_mismatch"

// An import file whose format_version this server does not read. Its own code, because
// the fix is different from every other 422 here: not a value to correct but a file from
// a newer (or older) server to re-export. Every other refusal of a file is the derived
// unprocessable_entity, with errors[0].location naming the field.
const CodeAspectFormatUnsupported = "aspect_format_unsupported"

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
	KeyedInput
	Body struct {
		TeamID string `json:"team_id" required:"true" format:"uuid"`
		Name   string `json:"name" required:"true" minLength:"1"`
	}
}

// The portable form (LAM-110, docs/aspect-type-format.md). No ids anywhere in it, and
// `additionalProperties: false` on both objects: a key this version does not know is a
// 422 naming it, not a value silently dropped on the way to a team that then wonders
// where it went.
type portableFieldBody struct {
	_ struct{} `additionalProperties:"false"`

	Label    string `json:"label" required:"true" minLength:"1" maxLength:"100" doc:"At most 100 characters, and not only whitespace."`
	Position int    `json:"position" required:"true" minimum:"0" maximum:"1000000" doc:"Ordering only: import sorts by it (ties keep file order) and renumbers 1..n, so gaps and repeats are fine."`
}

type portableBody struct {
	_ struct{} `additionalProperties:"false"`

	FormatVersion int                 `json:"format_version" required:"true" doc:"Always 1 for now. Any other number is a 422 aspect_format_unsupported at body.format_version."`
	Name          string              `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"At most 100 characters, and not only whitespace."`
	Fields        []portableFieldBody `json:"fields" required:"true" maxItems:"100" doc:"At most 100, in the order they should appear. May be empty."`
}

type exportAspectTypeOutput struct {
	Body portableBody
}

type importAspectTypeInput struct {
	KeyedInput
	Body struct {
		_ struct{} `additionalProperties:"false"`

		TeamID        string              `json:"team_id" required:"true" format:"uuid" doc:"The team that gets the new type."`
		FormatVersion int                 `json:"format_version" required:"true" doc:"Always 1 for now. Any other number is a 422 aspect_format_unsupported at body.format_version."`
		Name          string              `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"At most 100 characters, and not only whitespace."`
		Fields        []portableFieldBody `json:"fields" required:"true" maxItems:"100" doc:"At most 100, in the order they should appear. May be empty."`
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
	KeyedInput
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

type fieldUsageOutput struct {
	Body struct {
		DocumentCount int `json:"document_count" doc:"Documents of this type that hold a value under the field's id, archived ones included. Zero means removing the field loses nothing."`
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

// importError maps a refused file onto a 422 whose errors[0].location is the field.
// Nil for anything else, so the caller falls through to aspectError.
func importError(err error) *Error {
	var fe *aspect.FormatError
	if errors.As(err, &fe) {
		code := defaultCode(http.StatusUnprocessableEntity)
		if fe.Unsupported {
			code = CodeAspectFormatUnsupported
		}

		return newFieldError(code, "the file was not accepted: "+fe.Field+" "+fe.Message, "body."+fe.Field, fe.Message)
	}

	return limitError(err)
}

/*
┌─ api ───────────────────────────────────────────
│  registers the aspect type editor's operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *aspect.Service    nil only for cmd/openapi
│      docs   *document.Service  counts a field's use; nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/aspect-types/{id}/fields
*/

func registerAspectTypes(api huma.API, svc *aspect.Service, docs *document.Service, idem *idempotency.Store) {

	huma.Register(api, huma.Operation{
		OperationID: "create-aspect-type",
		Method:      http.MethodPost,
		Path:        V1 + "/aspect-types",
		Summary:     "Create an aspect type",
		Description: "Created with no fields. An aspect type with none is legal - it renders as a document with a title and nothing else.",
		Metadata:    needs(auth.AspectWrite),
	}, func(ctx context.Context, in *createAspectTypeInput) (*createdOutput[aspectTypeBody], error) {
		me, _ := auth.FromContext(ctx)

		t, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "create-aspect-type", aspect.ErrNotFound,
			func(ctx context.Context) (aspect.Type, string, error) {
				t, err := svc.CreateType(ctx, me.AccountID, in.Body.TeamID, in.Body.Name)
				return t, t.ID, err
			},
			func(ctx context.Context, id string) (aspect.Type, error) { return svc.Get(ctx, me.AccountID, id) })
		if err != nil {
			return nil, aspectError(err)
		}

		return created(in.key(), replayed, asAspectTypeBody(t)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-aspect-types",
		Method:      http.MethodGet,
		Path:        V1 + "/aspect-types",
		Summary:     "List a team's aspect types",
		Description: "Names only - fields are empty here. Fetch one type to get its fields.",
		Metadata:    needs(auth.AspectRead),
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
		Metadata:    needs(auth.AspectRead),
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
		Metadata:    needs(auth.AspectWrite),
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
		Metadata:    needs(auth.AspectWrite),
	}, func(ctx context.Context, in *addFieldInput) (*createdOutput[fieldBody], error) {
		me, _ := auth.FromContext(ctx)

		f, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "add-aspect-type-field", aspect.ErrNotFound,
			func(ctx context.Context) (aspect.Field, string, error) {
				f, err := svc.AddField(ctx, me.AccountID, in.ID, in.Body.Label)
				return f, f.ID, err
			},
			func(ctx context.Context, id string) (aspect.Field, error) {
				t, err := svc.Get(ctx, me.AccountID, in.ID)
				if err != nil {
					return aspect.Field{}, err
				}
				for _, f := range t.Fields {
					if f.ID == id {
						return f, nil
					}
				}
				return aspect.Field{}, aspect.ErrNotFound
			})
		if err != nil {
			return nil, aspectError(err)
		}

		return created(in.key(), replayed, asFieldBody(f)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "export-aspect-type",
		Method:      http.MethodGet,
		Path:        V1 + "/aspect-types/{id}/export",
		Summary:     "Export an aspect type as portable JSON",
		Description: "The type's name and fields, in position order, with `format_version` and no ids - safe to hand to another team, " +
			"which gets fresh ids on import. Nothing is recorded or changed. Needs `aspect:read`. See docs/aspect-type-format.md.",
		Metadata: needs(auth.AspectRead),
	}, func(ctx context.Context, in *aspectTypeInput) (*exportAspectTypeOutput, error) {
		me, _ := auth.FromContext(ctx)

		p, err := svc.Export(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, aspectError(err)
		}

		out := &exportAspectTypeOutput{}
		out.Body = portableBody{FormatVersion: p.FormatVersion, Name: p.Name, Fields: make([]portableFieldBody, len(p.Fields))}
		for i, f := range p.Fields {
			out.Body.Fields[i] = portableFieldBody{Label: f.Label, Position: f.Position}
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "import-aspect-type",
		Method:      http.MethodPost,
		Path:        V1 + "/aspect-types/import",
		Summary:     "Create a new aspect type from portable JSON",
		Description: "Always creates a NEW type in `team_id`; it never replaces or edits an existing one. Fields get fresh ids, " +
			"in the file's position order. If the team already has a type with that name the new one is named \"<name> (2)\", " +
			"then \"(3)\" and so on - read the name in the response. A file is refused whole, as a 422 whose `errors[0].location` " +
			"is the field (`body.format_version`, `body.name`, `body.fields[2].label`, or an unknown key); nothing is created. " +
			"Needs `aspect:write`. See docs/aspect-type-format.md.",
		Metadata: needs(auth.AspectWrite),
	}, func(ctx context.Context, in *importAspectTypeInput) (*createdOutput[aspectTypeBody], error) {
		me, _ := auth.FromContext(ctx)

		p := aspect.Portable{
			FormatVersion: in.Body.FormatVersion,
			Name:          in.Body.Name,
			Fields:        make([]aspect.PortableField, len(in.Body.Fields)),
		}
		for i, f := range in.Body.Fields {
			p.Fields[i] = aspect.PortableField{Label: f.Label, Position: f.Position}
		}

		t, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "import-aspect-type", aspect.ErrNotFound,
			func(ctx context.Context) (aspect.Type, string, error) {
				t, err := svc.Import(ctx, me.AccountID, in.Body.TeamID, p)
				return t, t.ID, err
			},
			func(ctx context.Context, id string) (aspect.Type, error) { return svc.Get(ctx, me.AccountID, id) })
		if err != nil {
			if e := importError(err); e != nil {
				return nil, e
			}

			return nil, aspectError(err)
		}

		return created(in.key(), replayed, asAspectTypeBody(t)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "rename-aspect-type-field",
		Method:      http.MethodPut,
		Path:        V1 + "/aspect-types/{id}/fields/{field_id}",
		Summary:     "Rename a field",
		Description: "The field's id does not change, so every document keeps the values stored under it.",
		Metadata:    needs(auth.AspectWrite),
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
		Metadata: needs(auth.AspectWrite),
	}, func(ctx context.Context, in *fieldInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.RemoveField(ctx, me.AccountID, in.FieldID); err != nil {
			return nil, aspectError(err)
		}

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-aspect-type-field-usage",
		Method:      http.MethodGet,
		Path:        V1 + "/aspect-types/{id}/fields/{field_id}/usage",
		Summary:     "Count the documents that hold a value in a field",
		Description: "For the warning before a field is removed: removing it leaves the values in every body, unrendered, so this says how many there are. " +
			"A field of another type, or one already removed, is `aspect_type_not_found` like the type itself.",
		Metadata: needs(auth.AspectRead),
	}, func(ctx context.Context, in *fieldInput) (*fieldUsageOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, aspectError(err)
		}

		found := false
		for _, f := range t.Fields {
			found = found || f.ID == in.FieldID
		}
		if !found {
			return nil, aspectError(aspect.ErrNotFound)
		}

		n, err := docs.CountFieldUsage(ctx, me.AccountID, t.ID, in.FieldID)
		if err != nil {
			return nil, err
		}

		out := &fieldUsageOutput{}
		out.Body.DocumentCount = n

		return out, nil
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
		Metadata: needs(auth.AspectWrite),
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

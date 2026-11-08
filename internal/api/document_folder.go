/*
╔═ document_folder.go ══════════════════════════════════════════════════════════════════
║  http handlers · document folders (0053)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeFolderNotFound       const
║      CodeFolderCycle          const
║      CodeFolderNeedsProject   const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  five operations under /document-folders
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

const (
	// CodeFolderNotFound is a 404 for a folder id in the path that is absent or out of scope,
	// and a 422 at body.parent_id or body.folder_id for a folder that is not one of the
	// project's.
	CodeFolderNotFound = "folder_not_found"

	// CodeFolderCycle is a 422 at body.parent_id: a folder moved into itself or into one of
	// its own sub-folders.
	CodeFolderCycle = "folder_cycle"

	// CodeFolderNeedsProject is a 422 at body.folder_id: a document with no project cannot be
	// filed in a folder.
	CodeFolderNeedsProject = "folder_needs_project"
)

type folderBody struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	ParentID  *string   `json:"parent_id" doc:"The folder this one is inside, of the same project. Null at the project's top level."`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type folderOutput struct {
	Body folderBody
}

type createFolderInput struct {
	KeyedInput
	Body struct {
		ProjectID string  `json:"project_id" required:"true" format:"uuid" doc:"Not the caller's: 422 project_not_found at body.project_id."`
		ParentID  *string `json:"parent_id,omitempty" format:"uuid" nullable:"true" doc:"Make the folder inside this folder of the same project. Omit or null for the project's top level. Not one of the project's: 422 folder_not_found at body.parent_id."`
		Name      string  `json:"name" required:"true" minLength:"1" maxLength:"100" doc:"1 to 100 characters (not bytes), and not only whitespace. Need not be unique."`
	}
}

type listFoldersInput struct {
	Body struct {
		ProjectID *string `json:"project_id,omitempty" format:"uuid" nullable:"true" doc:"Only this project's folders. Omit for every folder the caller can see."`
	}
}

type folderInput struct {
	ID string `path:"id" format:"uuid"`
}

// moveParent is update's parent_id: absent leaves the folder where it is, null moves it to
// the project's top level, a uuid moves it inside that folder. See moveProject.
type moveParent struct {
	Sent bool
	ID   string
}

func (m *moveParent) UnmarshalJSON(b []byte) error {
	m.Sent = true
	if string(b) == "null" {
		m.ID = ""

		return nil
	}

	return json.Unmarshal(b, &m.ID)
}

func (moveParent) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type: "string", Format: "uuid", Nullable: true,
		Description: "Move the folder inside this folder of the same project, or send null for the project's top level. Omit to leave it where it is. Not one of the project's: 422 folder_not_found; the folder itself or one of its sub-folders: 422 folder_cycle. Both at body.parent_id.",
	}
}

type updateFolderInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name     *string    `json:"name,omitempty" minLength:"1" maxLength:"100"`
		ParentID moveParent `json:"parent_id,omitzero"`
	}
}

func asFolderBody(f document.Folder) folderBody {
	return folderBody{ID: f.ID, ProjectID: f.ProjectID, ParentID: f.ParentID, Name: f.Name,
		CreatedAt: f.CreatedAt.UTC(), UpdatedAt: f.UpdatedAt.UTC()}
}

func folderError(err error) error {
	if errors.Is(err, document.ErrFolderNotFound) {
		var fe *document.FieldError
		if !errors.As(err, &fe) {
			return NewError(http.StatusNotFound, CodeFolderNotFound, "no such folder")
		}
	}

	// The 422s - a parent of another project, a cycle, a project that is not the caller's -
	// are the document write's own, so they read the same from either endpoint.
	return documentError(err)
}

/*
┌─ api ───────────────────────────────────────────
│  registers the document folder operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *document.Service    nil only for cmd/openapi
│      feed    ticketFeed           publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/document-folders/list
*/

func registerDocumentFolders(api huma.API, svc *document.Service, feed ticketFeed, idem *idempotency.Store) {

	huma.Register(api, huma.Operation{
		OperationID: "create-document-folder",
		Method:      http.MethodPost,
		Path:        V1 + "/document-folders",
		Summary:     "Create a folder in a project's documents",
		Description: "A folder belongs to one project and may sit inside another folder of it. Announced as `document_folder.created`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createFolderInput) (*createdOutput[folderBody], error) {
		me, _ := auth.FromContext(ctx)

		f, replayed, err := keyed(ctx, idem, me.AccountID, in.key(), "create-document-folder", document.ErrFolderNotFound,
			func(ctx context.Context) (document.Folder, string, error) {
				f, err := svc.CreateFolder(ctx, me.AccountID, in.Body.ProjectID, strOrEmpty(in.Body.ParentID), in.Body.Name)
				return f, f.ID, err
			},
			func(ctx context.Context, id string) (document.Folder, error) {
				return svc.GetFolder(ctx, me.AccountID, id)
			})
		if err != nil {
			return nil, folderError(err)
		}

		if !replayed {
			feed.publishFolder(realtime.DocumentFolderCreated, me.AccountID, f)
		}

		return created(in.key(), replayed, asFolderBody(f)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-document-folders",
		Method:      http.MethodPost,
		Path:        V1 + "/document-folders/list",
		Summary:     "List document folders (a read - POST for the body)",
		Description: "By project, then name. Always one page: `next_cursor` is null and `has_more` false.",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *listFoldersInput) (*listOutput[folderBody], error) {
		me, _ := auth.FromContext(ctx)

		folders, err := svc.ListFolders(ctx, me.AccountID, strOrEmpty(in.Body.ProjectID))
		if err != nil {
			return nil, folderError(err)
		}

		out := &listOutput[folderBody]{}
		out.Body.Items = make([]folderBody, len(folders))
		for i, f := range folders {
			out.Body.Items[i] = asFolderBody(f)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-document-folder",
		Method:      http.MethodGet,
		Path:        V1 + "/document-folders/{id}",
		Summary:     "Fetch one document folder",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *folderInput) (*folderOutput, error) {
		me, _ := auth.FromContext(ctx)

		f, err := svc.GetFolder(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, folderError(err)
		}

		return &folderOutput{Body: asFolderBody(f)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-document-folder",
		Method:      http.MethodPut,
		Path:        V1 + "/document-folders/{id}",
		Summary:     "Rename or move a document folder",
		Description: "Each member is optional and an omitted one is left alone. A folder moves with everything in it, and only within its project. Announced as `document_folder.updated`.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateFolderInput) (*folderOutput, error) {
		me, _ := auth.FromContext(ctx)

		f, err := svc.UpdateFolder(ctx, me.AccountID, in.ID, document.FolderPatch{
			Name: in.Body.Name, MoveParent: in.Body.ParentID.Sent, ParentID: in.Body.ParentID.ID,
		})
		if err != nil {
			return nil, folderError(err)
		}

		feed.publishFolder(realtime.DocumentFolderUpdated, me.AccountID, f)

		return &folderOutput{Body: asFolderBody(f)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-document-folder",
		Method:      http.MethodDelete,
		Path:        V1 + "/document-folders/{id}",
		Summary:     "Delete a folder; what it held moves up a level",
		Description: "Deletes no documents. The folder's documents and sub-folders move into its parent (the project's top level for a top-level folder). " +
			"Each document moved is a write: its `version` rises and it is announced as `document.updated`; each sub-folder moved is announced as " +
			"`document_folder.updated`; then the folder itself as `document_folder.deleted`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *folderInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		deleted, err := svc.DeleteFolder(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, folderError(err)
		}

		for _, d := range deleted.Documents {
			feed.publishDocument(realtime.DocumentUpdated, me.AccountID, d)
		}
		for _, f := range deleted.Folders {
			feed.publishFolder(realtime.DocumentFolderUpdated, me.AccountID, f)
		}
		feed.publishFolder(realtime.DocumentFolderDeleted, me.AccountID, deleted.Folder)

		return nil, nil
	})
}

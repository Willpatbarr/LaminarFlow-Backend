/*
╔═ document.go ═════════════════════════════════════════════════════════════════════════
║  http handlers · document resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeDocumentNotFound       const
║      CodeDocumentConflict       const
║      CodeDocumentFieldUnknown   const
║      CodeAspectTypeNoFields     const
║      documentConflict           struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · get · update · archive · unarchive · list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/comment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
)

// One 404 for absent, archived and not-yours alike, for the reason ticket_not_found is.
const CodeDocumentNotFound = "document_not_found"

// The one 409 a document write carrying If-Match can get. Tickets have two (edited,
// archived); a document has one, and `current.archived_at` says which happened, because
// nothing yet needs the two told apart by code.
const CodeDocumentConflict = "document_conflict"

// The two 422s that are about a document's shape rather than a length. Each has its own
// code because each has its own input to fix: document_field_unknown puts the offending
// body key in the location (body.<key>), aspect_type_has_no_fields is at body.aspect_type_id.
// A team_id or project_id that is not the caller's reuses team_not_found and project_not_found
// at body.team_id and body.project_id.
const (
	CodeDocumentFieldUnknown = "document_field_unknown"
	CodeAspectTypeNoFields   = "aspect_type_has_no_fields"
)

type documentBody struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Type         string         `json:"type" enum:"normal,aspect"`
	AspectTypeID *string        `json:"aspect_type_id" doc:"Set exactly when type is aspect; null otherwise."`
	TeamID       *string        `json:"team_id" doc:"The team the document is filed under. Null for a project's or a workspace-level document."`
	ProjectID    *string        `json:"project_id" doc:"The project the document is filed under. Null for a team's or a workspace-level document."`
	Body         map[string]any `json:"body" doc:"Keyed by aspect type field id; each value is a Markdown string (docs/adr/0006). A normal document keeps its text under the key content. A field with no value is an absent key."`
	Version      int            `json:"version" minimum:"1" doc:"Starts at 1 and rises by exactly 1 on every update, archive and unarchive. Send it back as If-Match to make a write conditional; also in the ETag header."`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at" doc:"When the document last changed - an edit, an archive or a restore. For display; to compare versions use version."`
	ArchivedAt   *time.Time     `json:"archived_at" doc:"Null while live. Set on an archived document, which only a 409's current and a document.archived event ever show."`
}

// linkedTicketBody is a ticket as a document lists it (LAM-111).
type linkedTicketBody struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	ArchivedAt *time.Time `json:"archived_at" doc:"Null while live. A link survives an archive, so an archived ticket is listed with this set."`
}

// documentDetailBody is what GET /documents/{id} returns: the document plus the tickets linked
// to it. Only that read carries them; writes, lists, conflicts and events keep documentBody.
// DocumentBody is documentBody under an exported name so it can be embedded; see TicketBody.
type DocumentBody = documentBody

type documentDetailBody struct {
	DocumentBody
	Tickets []linkedTicketBody `json:"tickets" doc:"The tickets linked to this document, oldest link first, archived ones included. Never null; empty for none."`
}

type documentDetailOutput struct {
	ETag string `header:"ETag" doc:"The document's version in quotes, as If-Match takes it back."`
	Body documentDetailBody
}

type documentOutput struct {
	ETag string `header:"ETag" doc:"The document's version in quotes, as If-Match takes it back."`
	Body documentBody
}

// documentVersionOutput is the answer to a write with no body, as versionOutput is for
// tickets: DELETE and unarchive answer 204, and the header is how the client that just
// archived at version 4 learns the document is at 5.
type documentVersionOutput struct {
	ETag string `header:"ETag" doc:"The document's new version in quotes, as If-Match takes it back."`
}

// DocumentPreconditioned is Preconditioned with the words of a document. Exported for the
// reason Preconditioned is: huma skips unexported embedded fields.
type DocumentPreconditioned struct {
	IfMatch string `header:"If-Match" pattern:"^\"[1-9][0-9]{0,8}\"$" example:"\"4\"" doc:"Make the write conditional on the document still being at the version you last saw, written as the ETag is: the number in double quotes, for example \"4\". If the document has moved on - edited, or archived - the write is refused with a 409 document_conflict carrying the current copy, and nothing changes. Omit it for a plain last-write-wins write."`
}

func (p DocumentPreconditioned) ifVersion() (*int, error) {
	return Preconditioned(p).ifVersion()
}

func asDocumentBody(d document.Document) documentBody {
	return documentBody{
		ID:           d.ID,
		Title:        d.Title,
		Type:         d.Type,
		AspectTypeID: d.AspectTypeID,
		TeamID:       d.TeamID,
		ProjectID:    d.ProjectID,
		Body:         d.Body,
		Version:      d.Version,
		CreatedAt:    d.CreatedAt.UTC(),
		UpdatedAt:    d.UpdatedAt.UTC(),
		ArchivedAt:   utcOrNil(d.ArchivedAt),
	}
}

func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()

	return &u
}

func asDocumentOutput(d document.Document) *documentOutput {
	return &documentOutput{ETag: etag(d.Version), Body: asDocumentBody(d)}
}

type createDocumentInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the create safe to retry. The same key from the same account returns the document the first call created instead of creating another - the original's current state, with Idempotent-Replayed: true. 1 to 128 printable ASCII characters; a UUID is ideal. Keys are per account and never expire. A replay ignores the body it carries: send a new key for a different document. Omit it for a plain create."`
	Body           struct {
		Title        string            `json:"title,omitempty" maxLength:"200" doc:"Up to 200 characters (not bytes). Omit for an untitled document."`
		Type         string            `json:"type,omitempty" enum:"normal,aspect" default:"normal"`
		AspectTypeID *string           `json:"aspect_type_id,omitempty" format:"uuid" nullable:"true" doc:"Required when type is aspect, forbidden otherwise (422 at body.aspect_type_id). An aspect type that is absent or another workspace's is a 404 aspect_type_not_found."`
		TeamID       *string           `json:"team_id,omitempty" format:"uuid" nullable:"true" doc:"File the document under a team. At most one of team_id and project_id; neither is a workspace-level document. Fixed at creation. Not the caller's: 422 team_not_found at body.team_id."`
		ProjectID    *string           `json:"project_id,omitempty" format:"uuid" nullable:"true" doc:"File the document under a project. Not the caller's: 422 project_not_found at body.project_id."`
		Body         map[string]string `json:"body,omitempty" doc:"Field id to Markdown string. For an aspect document every key must be a field id of its type, or the answer is a 422 document_field_unknown at body.<key>. A normal document keeps its text under content."`
	}
}

// Resolve rejects the two shapes of the header huma cannot see; see createTicketInput.
func (in *createDocumentInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

func (in *createDocumentInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

// createDocumentOutput is documentOutput plus whether the response is a replay; 200 for
// both, for createTicketOutput's reason.
type createDocumentOutput struct {
	ETag     string `header:"ETag" doc:"The document's version in quotes, as If-Match takes it back."`
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used, nothing was created, and the body is the original document as it is now. false: this request created the document."`
	Body     documentBody
}

type getDocumentInput struct {
	ID string `path:"id" format:"uuid"`
}

type writeDocumentInput struct {
	ID string `path:"id" format:"uuid"`
	DocumentPreconditioned
}

// moveProject is update's project_id: three states a pointer cannot tell apart. Absent leaves
// the document where it is (so every client written before LAM-138 keeps working), null takes
// it out of its project, a uuid files it under that project.
type moveProject struct {
	Sent bool
	ID   string
}

func (m *moveProject) UnmarshalJSON(b []byte) error {
	m.Sent = true
	if string(b) == "null" {
		m.ID = ""

		return nil
	}

	return json.Unmarshal(b, &m.ID)
}

// Schema is what the OpenAPI document and the request validator see: an optional,
// nullable uuid, like the create's project_id.
func (moveProject) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type: "string", Format: "uuid", Nullable: true,
		Description: "Move the document to this project, or send null to make it team-wide (no project). Omit to leave it where it is. Filing under a project clears a team_id; null files it under the team of the project it leaves. Not the caller's: 422 project_not_found at body.project_id.",
	}
}

type updateDocumentInput struct {
	ID string `path:"id" format:"uuid"`
	DocumentPreconditioned
	Body struct {
		Title        string            `json:"title" maxLength:"200" doc:"Up to 200 characters (not bytes)."`
		Type         string            `json:"type,omitempty" enum:"normal,aspect" default:"normal"`
		ProjectID    moveProject       `json:"project_id,omitzero"`
		AspectTypeID *string           `json:"aspect_type_id,omitempty" format:"uuid" nullable:"true" doc:"As on create. Turning a normal document into an aspect one sets both type and this."`
		Body         map[string]string `json:"body,omitempty" doc:"The complete body: it replaces the stored one. When a normal document becomes an aspect one, its text (content, from here if sent, else the stored body) moves into the type's first field - by position, then label - unless this body fills that field itself; a type with no fields is a 422 aspect_type_has_no_fields at body.aspect_type_id."`
	}
}

type listDocumentsInput struct {
	Body struct {
		Limit        int     `json:"limit,omitempty" minimum:"1" maximum:"200" doc:"Rows per page. Omitted means 50. Over the maximum is rejected, not clamped."`
		Cursor       string  `json:"cursor,omitempty" doc:"next_cursor from the previous page. Opaque - do not construct or parse it."`
		ProjectID    *string `json:"project_id,omitempty" format:"uuid" nullable:"true" doc:"Only documents filed under this project."`
		TeamID       *string `json:"team_id,omitempty" format:"uuid" nullable:"true" doc:"Only documents filed under this team."`
		Type         string  `json:"type,omitempty" enum:"normal,aspect" doc:"Only documents of this type."`
		AspectTypeID *string `json:"aspect_type_id,omitempty" format:"uuid" nullable:"true" doc:"Only documents of this aspect type."`
		Archived     bool    `json:"archived,omitempty" doc:"List archived documents instead of live ones. Default false."`
	}
}

/*
┏━ documentConflict ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the 409 body: the envelope, plus the other side
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      huma.StatusError
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ErrorModel    huma.ErrorModel    RFC 9457, embedded and inlined
┃      Code          string             document_conflict
┃      Current       documentBody       the server's copy, archived or not
┃      ChangedAt     time.Time          the copy's updated_at
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      documentError                    one per refused write
*/

// documentConflict is ticketConflict's shape without changed_by, which needs a column
// document does not have (0035). current is the whole copy including an archived one, for
// ticketConflict's reason: a client holding unsynced edits needs the other side even when
// the other side is "gone", and this is the one door to an archived document.
type documentConflict struct {
	huma.ErrorModel
	Code      string       `json:"code" enum:"document_conflict" doc:"The document was edited or archived since the version you sent; current.archived_at says which." example:"document_conflict"`
	Current   documentBody `json:"current" doc:"The server's copy of the document right now, in the shape of GET /documents/{id} - including when it is archived, which GET never returns."`
	ChangedAt time.Time    `json:"changed_at" doc:"When the copy last changed: current.updated_at."`
}

// documentError maps the service's errors onto the envelope. Anything else is a fault on
// this side and must not be reported as a 404, or a caller retries forever against a
// database that is down.
func documentError(err error) error {
	switch {
	case errors.Is(err, document.ErrNotFound):
		return NewError(http.StatusNotFound, CodeDocumentNotFound, "no such document")
	case errors.Is(err, document.ErrAspectTypeNotFound):
		return NewError(http.StatusNotFound, CodeAspectNotFound, "no such aspect type")
	}

	var conflict *document.Conflict
	if errors.As(err, &conflict) {
		detail := "the document has changed since the version you sent"
		switch {
		case conflict.Archived && conflict.Replay:
			detail = "the document this Idempotency-Key created has since been archived"
		case conflict.Archived:
			detail = "the document has been archived since the version you sent"
		}

		return &documentConflict{
			ErrorModel: huma.ErrorModel{
				Status: http.StatusConflict,
				Title:  http.StatusText(http.StatusConflict),
				Detail: detail,
			},
			Code:      CodeDocumentConflict,
			Current:   asDocumentBody(conflict.Current),
			ChangedAt: conflict.Current.UpdatedAt.UTC(),
		}
	}

	var fe *document.FieldError
	if errors.As(err, &fe) {
		switch {
		case errors.Is(fe.Kind, document.ErrUnknownField):
			return newFieldError(CodeDocumentFieldUnknown, "the body names a field that is not one of the document's type",
				"body.body."+fe.Field, "not a field of this document's aspect type")
		case errors.Is(fe.Kind, document.ErrNoFields):
			return newFieldError(CodeAspectTypeNoFields, "the aspect type has no fields to move the document's text into",
				"body.aspect_type_id", "this aspect type has no fields; add one first")
		case errors.Is(fe.Kind, document.ErrTypeShape):
			return newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"aspect_type_id must be set exactly when type is aspect",
				"body.aspect_type_id", "required when type is aspect, and forbidden otherwise")
		case errors.Is(fe.Kind, document.ErrScopeNotFound) && fe.Field == "team_id":
			return newFieldError(CodeTeamNotFound, "the team is not one the caller can file a document under",
				"body.team_id", "no such team in this workspace")
		case errors.Is(fe.Kind, document.ErrScopeNotFound):
			return newFieldError(CodeProjectNotFound, "the project is not one the caller can file a document under",
				"body.project_id", "no such project in this workspace")
		case errors.Is(fe.Kind, document.ErrScopeAmbiguous):
			return newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"send at most one of team_id and project_id",
				"body", "team_id and project_id are mutually exclusive")
		}
	}

	if e := limitError(err); e != nil {
		return e
	}

	return err
}

// documentConflictResponses documents the 409 and the default on an operation that can
// raise one, for the reason conflictResponses does: huma derives its error schema from the
// plain Error, which has no room for current.
func documentConflictResponses(api huma.API, description string) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	problem := func(schema *huma.Schema) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{"application/problem+json": {Schema: schema}}
	}

	return map[string]*huma.Response{
		"409": {
			Description: description,
			Content:     problem(registry.Schema(reflect.TypeOf(documentConflict{}), true, "DocumentConflict")),
		},
		"default": {
			Description: "Error",
			Content:     problem(registry.Schema(reflect.TypeOf(Error{}), true, "Error")),
		},
	}
}

const documentReplayConflict = "Conflict. Only when Idempotency-Key was sent and had already created a document that has since been archived: code is document_conflict, current is that document with archived_at set. Nothing was written, and no second document exists - restore the original or discard the local copy."

const documentIfMatchConflict = "Conflict. Only when If-Match was sent: code is document_conflict, current is the server's copy (archived_at says if it was archived). Nothing was written."

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// bodyValues widens the request's string map to the service's, which holds decoded JSON.
func bodyValues(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}

/*
┌─ api ───────────────────────────────────────────
│  registers the document operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *document.Service    nil only for cmd/openapi
│      feed    ticketFeed           publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/documents, GET /api/v1/documents/{id}
*/

func registerDocuments(api huma.API, svc *document.Service, tickets *ticket.Service, comments *comment.Service, feed ticketFeed) {

	huma.Register(api, huma.Operation{
		OperationID: "create-document",
		Method:      http.MethodPost,
		Path:        V1 + "/documents",
		Summary:     "Create a document (safe to retry with Idempotency-Key)",
		Description: "A normal document keeps its text under `content`; an aspect document's body is keyed by the field ids of its aspect type. " +
			"The body is indexed for search in the same transaction. " +
			"Send `Idempotency-Key` to make the create safe to retry after a lost response. " +
			"The first request with a key creates the document and answers `Idempotent-Replayed: false`; " +
			"any later request with the same key from the same account creates nothing and answers " +
			"`Idempotent-Replayed: true` with the original document as it is now - same status, 200, as a " +
			"fresh create. A replay publishes nothing on the change feed. If the original has been archived " +
			"the answer is a 409 `document_conflict` carrying it. The body of a replay is ignored.",
		Metadata:  needs(auth.TicketWrite),
		Responses: documentConflictResponses(api, documentReplayConflict),
	}, func(ctx context.Context, in *createDocumentInput) (*createDocumentOutput, error) {
		me, _ := auth.FromContext(ctx)

		d, replayed, err := svc.WriteIdempotent(ctx, document.SaveParams{
			IdempotencyKey: in.key(),
			AccountID:      me.AccountID,
			Title:          in.Body.Title,
			Type:           in.Body.Type,
			AspectTypeID:   strOrEmpty(in.Body.AspectTypeID),
			TeamID:         strOrEmpty(in.Body.TeamID),
			ProjectID:      strOrEmpty(in.Body.ProjectID),
			Body:           bodyValues(in.Body.Body),
		})
		if err != nil {
			return nil, documentError(err)
		}

		// A replay wrote nothing, so there is nothing to announce; see create-ticket.
		if !replayed {
			feed.publishDocument(realtime.DocumentCreated, me.AccountID, d)
		}

		out := &createDocumentOutput{ETag: etag(d.Version), Body: asDocumentBody(d)}
		if in.key() != nil {
			out.Replayed = strconv.FormatBool(replayed)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-document",
		Method:      http.MethodGet,
		Path:        V1 + "/documents/{id}",
		Summary:     "Fetch one document",
		Metadata:    needs(auth.TicketRead),
	}, func(ctx context.Context, in *getDocumentInput) (*documentDetailOutput, error) {
		me, _ := auth.FromContext(ctx)

		d, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, documentError(err)
		}

		// The document is read first: it is the check that the caller may see this document
		// at all, and LinkedTickets scopes only the tickets.
		linked, err := tickets.LinkedTickets(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, err
		}

		out := make([]linkedTicketBody, len(linked))
		for i, t := range linked {
			out[i] = linkedTicketBody{ID: t.ID, Title: t.Title, ArchivedAt: utcOrNil(t.ArchivedAt)}
		}

		return &documentDetailOutput{ETag: etag(d.Version), Body: documentDetailBody{DocumentBody: asDocumentBody(d), Tickets: out}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-document",
		Method:      http.MethodPut,
		Path:        V1 + "/documents/{id}",
		Summary:     "Replace a document's title, type and body",
		Description: "PUT replaces wholesale, so send the complete body. Last write wins unless `If-Match` is sent; with it the write happens only if the document is still at that version, otherwise it is a 409 `document_conflict` carrying the current copy. " +
			"Without it, a write to an archived document is `document_not_found`. A document's team is fixed at creation; its project can be changed with `project_id` (a uuid, or null for a team-wide document with no project), and is left alone when `project_id` is omitted. " +
			"Sending `type: aspect` with an `aspect_type_id` on a normal document turns it into an aspect document: see `body`.",
		Metadata:  needs(auth.TicketWrite),
		Responses: documentConflictResponses(api, documentIfMatchConflict),
	}, func(ctx context.Context, in *updateDocumentInput) (*documentOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		d, err := svc.Write(ctx, document.SaveParams{
			AccountID:    me.AccountID,
			ID:           in.ID,
			Title:        in.Body.Title,
			Type:         in.Body.Type,
			AspectTypeID: strOrEmpty(in.Body.AspectTypeID),
			Body:         bodyValues(in.Body.Body),
			MoveProject:  in.Body.ProjectID.Sent,
			ProjectID:    in.Body.ProjectID.ID,
			IfVersion:    ifVersion,
		})
		if err != nil {
			return nil, documentError(err)
		}

		feed.publishDocument(realtime.DocumentUpdated, me.AccountID, d)

		// Approvals this write made stale (LAM-122), announced after the commit like the rest.
		for _, id := range d.StaledReviews {
			if r, err := comments.ReviewByID(ctx, id); err == nil {
				feed.publishReviewRequest(realtime.ReviewRequestUpdated, me.AccountID, r)
			} else if c, err := comments.CommentByID(ctx, id); err == nil {
				// A plain comment re-anchored by a conversion (LAM-136).
				feed.publishComment(realtime.CommentUpdated, me.AccountID, c)
			}
		}

		return asDocumentOutput(d), nil
	})

	// DELETE archives, as it does for tickets: the row, and any comments on it, survive.
	huma.Register(api, huma.Operation{
		OperationID: "archive-document",
		Method:      http.MethodDelete,
		Path:        V1 + "/documents/{id}",
		Summary:     "Archive a document (soft delete - the row survives)",
		Description: "Takes `If-Match` like the update does. The archived document drops out of reads, lists and search. An already-archived document answers `document_conflict` when `If-Match` is sent and `document_not_found` when it is not. The new version is in the `ETag` header.",
		Metadata:    needs(auth.TicketWrite),
		Responses:   documentConflictResponses(api, documentIfMatchConflict),
	}, func(ctx context.Context, in *writeDocumentInput) (*documentVersionOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		d, err := svc.Archive(ctx, me.AccountID, in.ID, ifVersion)
		if err != nil {
			return nil, documentError(err)
		}

		feed.publishDocument(realtime.DocumentArchived, me.AccountID, d)

		return &documentVersionOutput{ETag: etag(d.Version)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "unarchive-document",
		Method:      http.MethodPost,
		Path:        V1 + "/documents/{id}/unarchive",
		Summary:     "Restore an archived document",
		Description: "Takes `If-Match`: the version of the archived document you last saw. Restoring a document that is not archived is `document_not_found`. It is indexed for search again. The new version is in the `ETag` header.",
		Metadata:    needs(auth.TicketWrite),
		Responses:   documentConflictResponses(api, documentIfMatchConflict),
	}, func(ctx context.Context, in *writeDocumentInput) (*documentVersionOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		d, err := svc.Unarchive(ctx, me.AccountID, in.ID, ifVersion)
		if err != nil {
			return nil, documentError(err)
		}

		feed.publishDocument(realtime.DocumentUnarchived, me.AccountID, d)

		return &documentVersionOutput{ETag: etag(d.Version)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-documents",
		Method:      http.MethodPost,
		Path:        V1 + "/documents/list",
		Summary:     "List documents (a read - see the description for why it is a POST)",
		Description: "Reads a page, most recently updated first. POST for the reason `list-tickets` is; nothing is written. " +
			"Filters are the optional members `project_id`, `team_id`, `type` and `aspect_type_id`, all of which must match. " +
			"Live documents only unless `archived` is true, which lists archived ones only.\n\n" +
			"Pagination is keyset: pass the previous page's `next_cursor` back as `cursor`. A cursor is only valid for the filters it was minted under; changing one restarts from the first page.",
		Metadata: needs(auth.TicketRead),
	}, func(ctx context.Context, in *listDocumentsInput) (*listOutput[documentBody], error) {
		me, _ := auth.FromContext(ctx)

		page, err := svc.List(ctx, me.AccountID, document.ListParams{
			ProjectID:    strOrEmpty(in.Body.ProjectID),
			TeamID:       strOrEmpty(in.Body.TeamID),
			Type:         in.Body.Type,
			AspectTypeID: strOrEmpty(in.Body.AspectTypeID),
			Archived:     in.Body.Archived,
			Limit:        in.Body.Limit,
			Cursor:       in.Body.Cursor,
		})
		if err != nil {
			return nil, listError(err)
		}

		out := &listOutput[documentBody]{Body: listEnvelope[documentBody]{
			Items:   make([]documentBody, len(page.Documents)),
			HasMore: page.HasMore,
		}}
		for i, d := range page.Documents {
			out.Body.Items[i] = asDocumentBody(d)
		}
		if page.NextCursor != "" {
			out.Body.NextCursor = &page.NextCursor
		}

		return out, nil
	})
}

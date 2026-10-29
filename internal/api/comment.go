/*
╔═ comment.go ══════════════════════════════════════════════════════════════════════════
║  http handlers · comment resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeCommentNotFound    const
║      CodeCommentNotAuthor   const
║      commentBody            struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · update · delete · list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/comment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"

	"github.com/danielgtaylor/huma/v2"
)

// 404 for a comment that is absent or whose parent the caller cannot reach, indistinguishable
// on purpose (ticket_not_found's reason). 403 is only for a comment the caller can read and
// did not write.
const (
	CodeCommentNotFound  = "comment_not_found"
	CodeCommentNotAuthor = "comment_not_author"
)

type commentBody struct {
	ID           string    `json:"id"`
	TicketID     *string   `json:"ticket_id" doc:"The ticket commented on. Exactly one of ticket_id and document_id is set."`
	DocumentID   *string   `json:"document_id" doc:"The document commented on."`
	FieldID      *string   `json:"field_id" doc:"The aspect field of the document the comment is anchored to; null for a whole-document or ticket comment, and after that field is removed from the type."`
	RegionStart  *int      `json:"region_start" doc:"Start of the highlighted range, a character offset into the anchored content. Null with region_end, or both set."`
	RegionEnd    *int      `json:"region_end" doc:"End of the range; equal to region_start for a caret."`
	AuthorID     *string   `json:"author_id" doc:"Null when the author's account has been deleted: render an authorless comment."`
	Body         string    `json:"body"`
	ReviewStatus *string   `json:"review_status" enum:"pending,approved,changes_requested" nullable:"true" doc:"Null for a plain comment, which is everything these endpoints create. A non-null value marks a review request (LAM-113)."`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" doc:"When the body last changed."`
}

func asCommentBody(c comment.Comment) commentBody {
	return commentBody{
		ID:           c.ID,
		TicketID:     c.TicketID,
		DocumentID:   c.DocumentID,
		FieldID:      c.FieldID,
		RegionStart:  c.RegionStart,
		RegionEnd:    c.RegionEnd,
		AuthorID:     c.AuthorID,
		Body:         c.Body,
		ReviewStatus: c.ReviewStatus,
		CreatedAt:    c.CreatedAt.UTC(),
		UpdatedAt:    c.UpdatedAt.UTC(),
	}
}

type createCommentInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the create safe to retry. The same key from the same account returns the comment the first call created instead of creating another, with Idempotent-Replayed: true. 1 to 128 printable ASCII characters; a UUID is ideal. Keys are per account and live as long as the comment: once it is deleted the key is free again. A replay ignores the body it carries. Omit it for a plain create."`
	Body           struct {
		TicketID    *string `json:"ticket_id,omitempty" format:"uuid" nullable:"true" doc:"The ticket to comment on. Exactly one of ticket_id and document_id."`
		DocumentID  *string `json:"document_id,omitempty" format:"uuid" nullable:"true" doc:"The document to comment on."`
		Body        string  `json:"body" minLength:"1" maxLength:"50000" doc:"Up to 50,000 characters (not bytes); not blank."`
		FieldID     *string `json:"field_id,omitempty" format:"uuid" nullable:"true" doc:"Anchor the comment to one aspect field. Documents only, and the field must belong to the document's aspect type: 422 at body.field_id otherwise."`
		RegionStart *int    `json:"region_start,omitempty" minimum:"0" nullable:"true" doc:"With region_end, the range the comment is about. Both or neither; 0 <= region_start <= region_end (equal is a caret)."`
		RegionEnd   *int    `json:"region_end,omitempty" minimum:"0" nullable:"true"`
	}
}

func (in *createCommentInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

func (in *createCommentInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

type createCommentOutput struct {
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used, nothing was created, and the body is the original comment as it is now. false: this request created the comment."`
	Body     commentBody
}

type commentOutput struct {
	Body commentBody
}

type updateCommentInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Body string `json:"body" minLength:"1" maxLength:"50000" doc:"The new text, up to 50,000 characters; not blank. The only thing an edit changes."`
	}
}

type deleteCommentInput struct {
	ID string `path:"id" format:"uuid"`
}

func commentError(err error) error {
	switch {
	case errors.Is(err, comment.ErrNotFound):
		return NewError(http.StatusNotFound, CodeCommentNotFound, "no such comment")
	case errors.Is(err, comment.ErrNotAuthor):
		return NewError(http.StatusForbidden, CodeCommentNotAuthor, "only the author may change or delete a comment")
	case errors.Is(err, comment.ErrTargetNotFound):
		return NewError(http.StatusNotFound, defaultCode(http.StatusNotFound), "no such ticket or document")
	}

	var fe *comment.FieldError
	if errors.As(err, &fe) {
		if errors.Is(fe.Kind, comment.ErrTargetShape) {
			return newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"send exactly one of ticket_id and document_id", "body", "exactly one of ticket_id and document_id is required")
		}

		if fe.Field == "Idempotency-Key" {
			return newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"the Idempotency-Key was not accepted", "header.Idempotency-Key", fe.Kind.Error())
		}

		return newFieldError(defaultCode(http.StatusUnprocessableEntity),
			"the "+fe.Field+" was not accepted", "body."+fe.Field, fe.Kind.Error())
	}

	if e := limitError(err); e != nil {
		return e
	}

	return err
}

// publishComment announces a comment as the REST body: comment.created, .updated and
// .deleted (the comment as it was). The workspace is the parent's. After the commit,
// best-effort, as above.
func (f ticketFeed) publishComment(kind, actorID string, c comment.Comment) {
	if f.hub == nil {
		return
	}

	e := realtime.Event{
		Type:      kind,
		ActorID:   actorID,
		At:        c.UpdatedAt.UTC(),
		CommentID: c.ID,
		Comment:   asCommentBody(c),
	}
	if c.TicketID != nil {
		e.TicketID = *c.TicketID
	}
	if c.DocumentID != nil {
		e.DocumentID = *c.DocumentID
	}
	if kind == realtime.CommentDeleted {
		e.At = time.Now().UTC()
	}

	f.hub.Publish(c.WorkspaceID, e)
}

/*
┌─ api ───────────────────────────────────────────
│  registers the comment operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *comment.Service    nil only for cmd/openapi
│      feed    ticketFeed          publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/comments, PUT /api/v1/comments/{id}
*/

// Tokens: reads need ticket:read and writes ticket:write. Comments live on tickets and
// documents, and documents already sit under the ticket scopes (create-document), so no new
// scope is minted - a script that may edit tickets may comment on them.
func registerComments(api huma.API, svc *comment.Service, feed ticketFeed) {
	huma.Register(api, huma.Operation{
		OperationID: "create-comment",
		Method:      http.MethodPost,
		Path:        V1 + "/comments",
		Summary:     "Comment on a ticket or a document (safe to retry with Idempotency-Key)",
		Description: "A plain comment: `review_status` is null and not accepted here. Send exactly one of `ticket_id` and `document_id`. " +
			"`field_id` anchors it to one field of a document and must belong to the document's aspect type; `region_start`/`region_end` mark a range, both or neither. " +
			"A target the caller cannot see (absent, archived, another workspace) is a 404. " +
			"The comment is indexed for search in the same transaction. " +
			"Send `Idempotency-Key` to make the create safe to retry after a lost response: the first request with a key creates the comment and answers `Idempotent-Replayed: false`; " +
			"any later request with the same key from the same account creates nothing and answers `Idempotent-Replayed: true` with the original comment, same status 200. A replay publishes nothing on the change feed and ignores its body.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createCommentInput) (*createCommentOutput, error) {
		me, _ := auth.FromContext(ctx)

		c, replayed, err := svc.Create(ctx, me.AccountID, comment.CreateParams{
			TicketID:       in.Body.TicketID,
			DocumentID:     in.Body.DocumentID,
			Body:           in.Body.Body,
			FieldID:        in.Body.FieldID,
			RegionStart:    in.Body.RegionStart,
			RegionEnd:      in.Body.RegionEnd,
			IdempotencyKey: in.key(),
		})
		if err != nil {
			return nil, commentError(err)
		}

		// A replay wrote nothing, so there is nothing to announce.
		if !replayed {
			feed.publishComment(realtime.CommentCreated, me.AccountID, c)
		}

		out := &createCommentOutput{Body: asCommentBody(c)}
		if in.key() != nil {
			out.Replayed = strconv.FormatBool(replayed)
		}

		return out, nil
	})

	registerList(api, listSpec[commentBody]{
		OperationID: "list-comments",
		Permission:  auth.TicketRead,
		Path:        V1 + "/comments/list",
		Summary:     "List comments (a read - see the description for why it is a POST)",
		Fields:      comment.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[commentBody], error) {
			p := comment.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.List(ctx, accountID, p)
			if err != nil {
				return listPage[commentBody]{}, err
			}

			items := make([]commentBody, len(page.Comments))
			for i, c := range page.Comments {
				items[i] = asCommentBody(c)
			}

			return listPage[commentBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-comment",
		Method:      http.MethodPut,
		Path:        V1 + "/comments/{id}",
		Summary:     "Edit a comment's body (author only)",
		Description: "Changes `body` and nothing else; the target, anchor and region are fixed at creation. A comment whose author's account was deleted can be edited by nobody. Reindexed for search in the same transaction.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateCommentInput) (*commentOutput, error) {
		me, _ := auth.FromContext(ctx)

		c, err := svc.Update(ctx, me.AccountID, in.ID, in.Body.Body)
		if err != nil {
			return nil, commentError(err)
		}

		feed.publishComment(realtime.CommentUpdated, me.AccountID, c)

		return &commentOutput{Body: asCommentBody(c)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-comment",
		Method:      http.MethodDelete,
		Path:        V1 + "/comments/{id}",
		Summary:     "Delete a comment (author only)",
		Description: "A hard delete: a comment has no archive state. Removed from search in the same transaction.",
		Metadata:    needs(auth.TicketWrite),
	}, func(ctx context.Context, in *deleteCommentInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		c, err := svc.Delete(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, commentError(err)
		}

		feed.publishComment(realtime.CommentDeleted, me.AccountID, c)

		return nil, nil
	})
}

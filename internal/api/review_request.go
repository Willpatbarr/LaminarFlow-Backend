/*
╔═ review_request.go ═══════════════════════════════════════════════════════════════════
║  http handlers · review request resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeReviewRequestNotFound    const
║      CodeNotAReviewer             const
║      reviewRequestBody            struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · list · update
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

// 404 for a request that is absent, is a plain comment, or whose document the caller cannot
// reach, indistinguishable on purpose. 403 only for a request the caller can read and is not
// a reviewer of.
const (
	CodeReviewRequestNotFound = "review_request_not_found"
	CodeNotAReviewer          = "not_a_reviewer"
)

type reviewerBody struct {
	AccountID string `json:"account_id" format:"uuid"`
}

type reviewRequestBody struct {
	ID           string         `json:"id"`
	DocumentID   string         `json:"document_id"`
	FieldID      *string        `json:"field_id" nullable:"true" doc:"The aspect field the request is anchored to. Null for a normal document (the range is in its content), and after the field was removed from the aspect type; the request and its offsets are kept."`
	RegionStart  int            `json:"region_start" doc:"Start of the range, in characters (Unicode code points) of the field's stored Markdown string, counting from 0. See docs/review-requests.md."`
	RegionEnd    int            `json:"region_end" doc:"End of the range, exclusive; equal to region_start for a caret."`
	AnchorText   *string        `json:"anchor_text" nullable:"true" doc:"The text of the field between region_start and region_end when the request was made. A snapshot: it does not follow later edits of the document."`
	StaleReason  *string        `json:"stale_reason" nullable:"true" doc:"Why an approved request went back to pending: \"anchored text changed\" when a document edit changed more of the anchored text than review.stale_threshold_percent allows (docs/review-requests.md). Null otherwise; cleared when a reviewer approves or requests changes."`
	Body         string         `json:"body" doc:"The note."`
	ReviewStatus string         `json:"review_status" enum:"pending,approved,changes_requested" doc:"The request's overall state. There is no per-reviewer verdict: a reviewer's action sets this."`
	AuthorID     *string        `json:"author_id" nullable:"true" doc:"Null when the author's account was deleted."`
	Reviewers    []reviewerBody `json:"reviewers"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at" doc:"When the body or the review_status last changed (or it went stale). Not an edit marker: use edited_at."`
	EditedAt     *time.Time     `json:"edited_at" nullable:"true" doc:"When the note (body) was last edited, null if never. A status change does not set it."`
	DecidedBy    *string        `json:"decided_by" nullable:"true" format:"uuid" doc:"Who last approved or requested changes. Kept when the request goes back to pending, so a client can say who approved before it went stale. Null if it was never decided, was decided before this was recorded, or that account was deleted."`
	DecidedAt    *time.Time     `json:"decided_at" nullable:"true" doc:"When that decision was made. Null if never decided."`
	DecisionNote *string        `json:"decision_note" nullable:"true" doc:"The optional note that came with that decision. Null if none was given."`
}

func asReviewRequestBody(r comment.ReviewRequest) reviewRequestBody {
	b := reviewRequestBody{
		ID:           r.ID,
		FieldID:      r.FieldID,
		AnchorText:   r.AnchorText,
		StaleReason:  r.StaleReason,
		Body:         r.Body,
		AuthorID:     r.AuthorID,
		Reviewers:    make([]reviewerBody, len(r.Reviewers)),
		CreatedAt:    r.CreatedAt.UTC(),
		UpdatedAt:    r.UpdatedAt.UTC(),
		EditedAt:     utcPtr(r.EditedAt),
		DecidedBy:    r.DecidedBy,
		DecidedAt:    utcPtr(r.DecidedAt),
		DecisionNote: r.DecisionNote,
	}
	if r.DocumentID != nil {
		b.DocumentID = *r.DocumentID
	}
	if r.RegionStart != nil {
		b.RegionStart = *r.RegionStart
	}
	if r.RegionEnd != nil {
		b.RegionEnd = *r.RegionEnd
	}
	if r.ReviewStatus != nil {
		b.ReviewStatus = *r.ReviewStatus
	}
	for i, id := range r.Reviewers {
		b.Reviewers[i] = reviewerBody{AccountID: id}
	}

	return b
}

type createReviewRequestInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the create safe to retry, exactly as on POST /comments (and sharing its keys: a key already used by a plain comment of yours is a 422). The same key from the same account returns the request as it is now with Idempotent-Replayed: true and creates and publishes nothing; a replay ignores the body it carries."`
	Body           struct {
		DocumentID  string   `json:"document_id" format:"uuid" doc:"The document to review."`
		FieldID     *string  `json:"field_id,omitempty" format:"uuid" nullable:"true" doc:"Required for an aspect document: the field the range is in, which must belong to its aspect type. Omit it for a normal document, whose range is in its content (body key content). 422 at body.field_id otherwise."`
		RegionStart int      `json:"region_start" minimum:"0" doc:"Start of the range: a character offset into the field's stored Markdown string. See docs/review-requests.md."`
		RegionEnd   int      `json:"region_end" minimum:"0" doc:"End of the range, exclusive; 0 <= region_start <= region_end <= the field's length in characters (equal to region_start is a caret). 422 at body.region_end otherwise."`
		Body        string   `json:"body" minLength:"1" maxLength:"50000" doc:"The note to the reviewers. Up to 50,000 characters; not blank."`
		ReviewerIDs []string `json:"reviewer_ids" minItems:"1" maxItems:"50" items.format:"uuid" doc:"The accounts asked to review: at least one, each a member of the document's workspace and listed once. 422 at body.reviewer_ids[i] for the first that is not."`
	}
}

type reviewRequestOutput struct {
	Body reviewRequestBody
}

type createReviewRequestOutput struct {
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used, nothing was created, and the body is the original request as it is now."`
	Body     reviewRequestBody
}

func (in *createReviewRequestInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

func (in *createReviewRequestInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

type updateReviewRequestInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		ReviewStatus string `json:"review_status" enum:"approved,changes_requested,pending" doc:"The new overall state. Only a listed reviewer may set it. Null is not accepted."`
		DecisionNote string `json:"decision_note,omitempty" maxLength:"50000" doc:"Optional short note for the requester, sent with approved or changes_requested and stored as decision_note (blank means none). 422 at body.decision_note with pending. Ignored, like the rest of the call, when the request already has that status, so a retry is safe."`
	}
}

func reviewRequestError(err error) error {
	switch {
	case errors.Is(err, comment.ErrNotFound):
		return NewError(http.StatusNotFound, CodeReviewRequestNotFound, "no such review request")
	case errors.Is(err, comment.ErrNotReviewer):
		return NewError(http.StatusForbidden, CodeNotAReviewer, "only a reviewer of this request may change its status")
	}

	return commentError(err)
}

// publishReviewRequest announces a review request as the REST body, in the document's
// workspace, after the commit and best-effort like the others.
func (f ticketFeed) publishReviewRequest(kind, actorID string, r comment.ReviewRequest) {
	if f.hub == nil {
		return
	}

	e := realtime.Event{
		Type:          kind,
		ActorID:       actorID,
		At:            r.UpdatedAt.UTC(),
		CommentID:     r.ID,
		ReviewRequest: asReviewRequestBody(r),
	}
	if r.DocumentID != nil {
		e.DocumentID = *r.DocumentID
	}

	f.hub.Publish(r.WorkspaceID, e)
}

/*
┌─ api ───────────────────────────────────────────
│  registers the review request operations
├─ in ────────────────────────────────────────────
│      api     huma.API
│      svc     *comment.Service    nil only for cmd/openapi
│      feed    ticketFeed
├─ example ───────────────────────────────────────
│      →  POST /api/v1/review-requests
*/

// Tokens use the comment scopes: ticket:write to create and decide, ticket:read to list. The
// AI-agent use case (Master Spec 6.3) is a token holding ticket:write.
func registerReviewRequests(api huma.API, svc *comment.Service, feed ticketFeed) {
	huma.Register(api, huma.Operation{
		OperationID: "create-review-request",
		Method:      http.MethodPost,
		Path:        V1 + "/review-requests",
		Summary:     "Ask for a review of a range of a document field",
		Description: "Creates a comment on the document with `review_status` `pending`, anchored to the range `region_start`..`region_end`, and attaches the reviewers, in one transaction. " +
			"On an aspect document the range is in `field_id`; on a normal document send no `field_id` and it is in `content`. Offsets count characters (Unicode code points) of that stored Markdown string; the range must lie inside the field's current text. `anchor_text` records what was in the range. " +
			"Each reviewer must be a member of the document's workspace. A document the caller cannot see (absent, archived, another workspace) is a 404. " +
			"The request is also a comment: it is returned by `POST /comments/list`, its note is edited and the request deleted through `/comments/{id}` by its author, and it is indexed for search. " +
			"Publishes `review_request.created`. Send `Idempotency-Key` to make the create safe to retry, as on `POST /comments`. Callable with an API token holding `ticket:write`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *createReviewRequestInput) (*createReviewRequestOutput, error) {
		me, _ := auth.FromContext(ctx)

		r, replayed, err := svc.CreateReview(ctx, me.AccountID, comment.ReviewParams{
			DocumentID:  in.Body.DocumentID,
			FieldID:     in.Body.FieldID,
			RegionStart: in.Body.RegionStart,
			RegionEnd:   in.Body.RegionEnd,
			Body:        in.Body.Body,
			ReviewerIDs: in.Body.ReviewerIDs,

			IdempotencyKey: in.key(),
		})
		if err != nil {
			return nil, reviewRequestError(err)
		}

		if !replayed {
			feed.publishReviewRequest(realtime.ReviewRequestCreated, me.AccountID, r)
		}

		out := &createReviewRequestOutput{Body: asReviewRequestBody(r)}
		if in.key() != nil {
			out.Replayed = strconv.FormatBool(replayed)
		}

		return out, nil
	})

	registerList(api, listSpec[reviewRequestBody]{
		OperationID: "list-review-requests",
		Permission:  auth.TicketRead,
		Path:        V1 + "/review-requests/list",
		Summary:     "List review requests (a read - see the description for why it is a POST)",
		Fields:      comment.ReviewFields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[reviewRequestBody], error) {
			p := comment.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.ListReviews(ctx, accountID, p)
			if err != nil {
				return listPage[reviewRequestBody]{}, err
			}

			items := make([]reviewRequestBody, len(page.Requests))
			for i, r := range page.Requests {
				items[i] = asReviewRequestBody(r)
			}

			return listPage[reviewRequestBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-review-request",
		Method:      http.MethodPut,
		Path:        V1 + "/review-requests/{id}",
		Summary:     "Approve, request changes on, or reopen a review request (reviewers only)",
		Description: "Sets `review_status` to `approved`, `changes_requested` or `pending`. Only an account listed in `reviewers` may; anyone else who can see the request gets 403 `not_a_reviewer`. " +
			"There is one status for the whole request, not one per reviewer: the last reviewer to act decides it. Setting the status it already has is a 200 that writes and publishes nothing. " +
			"A change bumps `updated_at`, records `decided_by`, `decided_at` and `decision_note` (approved or changes_requested; reopening keeps them) and publishes `review_request.updated`. It does not set `edited_at`.",
		Metadata: needs(auth.TicketWrite),
	}, func(ctx context.Context, in *updateReviewRequestInput) (*reviewRequestOutput, error) {
		me, _ := auth.FromContext(ctx)

		r, changed, err := svc.SetReviewStatus(ctx, me.AccountID, in.ID, in.Body.ReviewStatus, in.Body.DecisionNote)
		if err != nil {
			return nil, reviewRequestError(err)
		}

		if changed {
			feed.publishReviewRequest(realtime.ReviewRequestUpdated, me.AccountID, r)
		}

		return &reviewRequestOutput{Body: asReviewRequestBody(r)}, nil
	})
}

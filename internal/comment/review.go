/*
╔═ review.go ═══════════════════════════════════════════════════════════════════════════
║  comment · review requests
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ReviewRequest      struct
║      ReviewParams       struct
║      ReviewFields       filter.Schema
║      ErrNotReviewer     error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  CreateReview · SetReviewStatus · ListReviews
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package comment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A review request is a comment row with review_status set and its reviewers in
// comment_reviewer (0017, 0018), so it is written here, by the one writer of both tables.
// It is a comment on a document, anchored to a range of one aspect field.

// Review statuses, the closed set 0017 enforces.
const (
	StatusPending          = "pending"
	StatusApproved         = "approved"
	StatusChangesRequested = "changes_requested"
)

// MaxReviewers bounds one request's reviewers. A request is a note to a handful of people;
// the bound keeps a create's membership check and a request's body small.
const MaxReviewers = 50

// ErrNotReviewer: the caller can see the request and is not one of its reviewers.
var ErrNotReviewer = errors.New("comment: not a reviewer")

// The reasons a review create or status change is refused as a 422.
var (
	ErrNoReviewers      = errors.New("at least one reviewer is required")
	ErrTooManyReviewers = errors.New("at most " + strconv.Itoa(MaxReviewers) + " reviewers")
	ErrDuplicateReview  = errors.New("listed twice")
	ErrNotAMember       = errors.New("not a member of the document's workspace")
	ErrRegionPastEnd    = errors.New("is past the end of the field's text")
	ErrUnknownStatus    = errors.New("must be pending, approved or changes_requested")
	ErrFieldRequired    = errors.New("is required for an aspect document")
	ErrKeyNotReview     = errors.New("was used by a comment that is not a review request")
)

/*
┏━ ReviewRequest ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one review request as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Comment       the row; ReviewStatus is never nil
┃      AnchorText    *string    the text of the range when it was made
┃      Reviewers     []string   account ids, sorted
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      CreateReview · SetReviewStatus · ListReviews
*/

type ReviewRequest struct {
	Comment
	AnchorText *string
	Reviewers  []string
}

func (r *ReviewRequest) dest() []any {
	return append(r.Comment.dest(), &r.AnchorText, &r.Reviewers)
}

const reviewColumns = columns + `, c.anchor_text,
	ARRAY(SELECT cr.account_id::text FROM comment_reviewer cr
	       WHERE cr.comment_id = c.id ORDER BY cr.account_id)`

// ReviewParams is a new review request. Offsets are characters (Unicode code points) of the
// stored Markdown string of the field; docs/review-requests.md.
type ReviewParams struct {
	DocumentID string
	// FieldID is required for an aspect document and must be nil for a normal one, whose
	// anchor is its document.ContentKey value.
	FieldID     *string
	RegionStart int
	RegionEnd   int
	Body        string
	ReviewerIDs []string

	// IdempotencyKey, when set, makes the create safe to retry; see CreateReview.
	IdempotencyKey *string
}

func (p ReviewParams) validate() error {
	if p.RegionStart < 0 {
		return &FieldError{Field: "region_start", Kind: ErrRegionRange}
	}
	if p.RegionEnd < p.RegionStart {
		return &FieldError{Field: "region_end", Kind: ErrRegionRange}
	}
	if err := textlimit.Required("body", p.Body, MaxBodyLength); err != nil {
		return err
	}
	if len(p.ReviewerIDs) == 0 {
		return &FieldError{Field: "reviewer_ids", Kind: ErrNoReviewers}
	}
	if len(p.ReviewerIDs) > MaxReviewers {
		return &FieldError{Field: "reviewer_ids", Kind: ErrTooManyReviewers}
	}
	seen := map[string]bool{}
	for i, id := range p.ReviewerIDs {
		id = strings.ToLower(id)
		if seen[id] {
			return &FieldError{Field: "reviewer_ids[" + strconv.Itoa(i) + "]", Kind: ErrDuplicateReview}
		}
		seen[id] = true
	}

	return nil
}

/*
┌─ comment ───────────────────────────────────────
│  creates a review request and its reviewers
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string          the author
│      p            ReviewParams
├─ out ───────────────────────────────────────────
│      ReviewRequest
│      bool                         true: the key had been used, nothing was written
│      error        *FieldError · *textlimit.FieldError · ErrTargetNotFound
*/

// A replay (same account, same IdempotencyKey) returns the request as it is now and writes
// nothing; the key shares comment's index (0040), so a key used by a plain comment is
// refused rather than answered with that comment. The key is looked up before the target is
// resolved, as Create does.
//
// The comment row (review_status 'pending', the anchor text snapshotted from the document's
// current field value) and every comment_reviewer row go in one transaction, and the request
// is indexed for search in it. The offsets are checked against the field's current text, so
// a range past its end is refused instead of stored dangling.
//
// The document is read, not locked: the snapshot is the text as of this read, and an edit
// that commits a moment later is the ordinary case LAM-122 exists for.
func (s *Service) CreateReview(ctx context.Context, accountID string, p ReviewParams) (ReviewRequest, bool, error) {
	if err := p.validate(); err != nil {
		return ReviewRequest{}, false, err
	}

	if p.IdempotencyKey != nil {
		if r, err := s.replay(ctx, s.pool, accountID, *p.IdempotencyKey); err == nil || !errors.Is(err, ErrNotFound) {
			return r, err == nil, err
		}
	}

	workspaceID, aspectTypeID, err := s.target(ctx, accountID, CreateParams{DocumentID: &p.DocumentID})
	if err != nil {
		return ReviewRequest{}, false, err
	}

	// A normal document has no fields: its one value is under ContentKey and the comment's
	// field_id stays null. An aspect document needs the field.
	key := document.ContentKey
	switch {
	case aspectTypeID == nil && p.FieldID != nil:
		return ReviewRequest{}, false, &FieldError{Field: "field_id", Kind: ErrFieldNotOnDoc}
	case aspectTypeID != nil && p.FieldID == nil:
		return ReviewRequest{}, false, &FieldError{Field: "field_id", Kind: ErrFieldRequired}
	case p.FieldID != nil:
		key = *p.FieldID
		if err := s.checkField(ctx, workspaceID, aspectTypeID, key); err != nil {
			return ReviewRequest{}, false, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.checkReviewers(ctx, tx, workspaceID, p.ReviewerIDs); err != nil {
		return ReviewRequest{}, false, err
	}

	var text string
	err = tx.QueryRow(ctx,
		`SELECT CASE WHEN jsonb_typeof(d.body -> $2::text) = 'string' THEN d.body ->> $2::text ELSE '' END
		   FROM document d WHERE d.id = $1::uuid`, p.DocumentID, key,
	).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, false, ErrTargetNotFound
	}
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: read field: %w", err)
	}

	runes := []rune(text)
	if p.RegionEnd > len(runes) {
		return ReviewRequest{}, false, &FieldError{Field: "region_end", Kind: ErrRegionPastEnd}
	}
	anchor := string(runes[p.RegionStart:p.RegionEnd])

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO comment (document_id, field_id, region_start, region_end, author_id, body,
		                      review_status, anchor_text, idempotency_key)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, $6, 'pending', $7, $8::text)
		 ON CONFLICT (author_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		 DO NOTHING
		 RETURNING id::text`,
		p.DocumentID, p.FieldID, p.RegionStart, p.RegionEnd, accountID, p.Body, anchor, p.IdempotencyKey,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// The key was used between the lookup and the insert; read the winner on this connection.
		if p.IdempotencyKey != nil {
			if r, err := s.replay(ctx, tx, accountID, *p.IdempotencyKey); err == nil {
				return r, true, nil
			}
		}

		return ReviewRequest{}, false, fmt.Errorf("comment: insert review request wrote nothing")
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23503" {
			switch pg.ConstraintName {
			case "comment_field_id_fkey":
				return ReviewRequest{}, false, &FieldError{Field: "field_id", Kind: ErrFieldNotOnDoc}
			case "comment_document_id_fkey":
				return ReviewRequest{}, false, ErrTargetNotFound
			}
		}

		return ReviewRequest{}, false, fmt.Errorf("comment: insert review request: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO comment_reviewer (comment_id, account_id)
		 SELECT $1::uuid, r FROM unnest($2::uuid[]) AS r`, id, p.ReviewerIDs,
	); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23503" {
			// A reviewer's account was deleted after the membership check.
			return ReviewRequest{}, false, &FieldError{Field: "reviewer_ids", Kind: ErrNotAMember}
		}

		return ReviewRequest{}, false, fmt.Errorf("comment: insert reviewers: %w", err)
	}

	if err := search.IndexComment(ctx, tx, id); err != nil {
		return ReviewRequest{}, false, err
	}

	out, err := s.reviewByID(ctx, tx, id)
	if err != nil {
		return ReviewRequest{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: commit: %w", err)
	}

	return out, false, nil
}

// checkReviewers proves every reviewer is a member of the document's workspace, naming the
// first one who is not by its position in the request.
func (s *Service) checkReviewers(ctx context.Context, tx pgx.Tx, workspaceID string, ids []string) error {
	rows, err := tx.Query(ctx,
		`SELECT account_id::text FROM workspace_member
		  WHERE workspace_id = $1::uuid AND account_id = ANY($2::uuid[])`, workspaceID, ids)
	if err != nil {
		return fmt.Errorf("comment: reviewer membership: %w", err)
	}
	defer rows.Close()

	members := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("comment: reviewer membership scan: %w", err)
		}
		members[id] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("comment: reviewer membership rows: %w", err)
	}

	for i, id := range ids {
		if !members[strings.ToLower(id)] {
			return &FieldError{Field: "reviewer_ids[" + strconv.Itoa(i) + "]", Kind: ErrNotAMember}
		}
	}

	return nil
}

// replay reads the review request the caller's key made. ErrNotFound: the key is unused.
func (s *Service) replay(ctx context.Context, q querier, accountID, key string) (ReviewRequest, error) {
	var r ReviewRequest
	var review bool
	err := q.QueryRow(ctx,
		`SELECT `+reviewColumns+`, c.review_status IS NOT NULL FROM `+from+`
		  WHERE c.author_id = $1::uuid AND c.idempotency_key = $2`, accountID, key,
	).Scan(append(r.dest(), &review)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("comment: review by key: %w", err)
	}
	if !review {
		return ReviewRequest{}, &FieldError{Field: "Idempotency-Key", Kind: ErrKeyNotReview}
	}

	return r, nil
}

// reviewByID reads one review request on the caller's own transaction or the pool. Not scoped
// by reach: callers have established that.
func (s *Service) reviewByID(ctx context.Context, q querier, id string) (ReviewRequest, error) {
	var r ReviewRequest
	err := q.QueryRow(ctx,
		`SELECT `+reviewColumns+` FROM `+from+` WHERE c.id = $1::uuid AND c.review_status IS NOT NULL`, id,
	).Scan(r.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("comment: read review request: %w", err)
	}

	return r, nil
}

/*
┌─ comment ───────────────────────────────────────
│  sets a review request's status
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string          must be one of the reviewers
│      id           string
│      status       string          pending · approved · changes_requested
├─ out ───────────────────────────────────────────
│      ReviewRequest
│      bool                         true: the status changed and was written
│      error        ErrNotFound · ErrNotReviewer · *FieldError
*/

// A plain comment, or one the caller cannot reach, is ErrNotFound. Setting the status it
// already has writes nothing (and so is not announced). A change bumps updated_at: for a
// review request, updated_at is the last time its body or its status changed.
func (s *Service) SetReviewStatus(ctx context.Context, accountID, id, status string) (ReviewRequest, bool, error) {
	switch status {
	case StatusPending, StatusApproved, StatusChangesRequested:
	default:
		return ReviewRequest{}, false, &FieldError{Field: "review_status", Kind: ErrUnknownStatus}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var r ReviewRequest
	err = tx.QueryRow(ctx,
		expand(`SELECT `+reviewColumns+` FROM `+from+`
		         WHERE c.id = $1::uuid AND c.review_status IS NOT NULL AND `+reachable+`
		           FOR UPDATE OF c`, "$2"),
		id, accountID,
	).Scan(r.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewRequest{}, false, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: lock review request: %w", err)
	}

	listed := false
	for _, a := range r.Reviewers {
		if a == accountID {
			listed = true
		}
	}
	if !listed {
		return ReviewRequest{}, false, ErrNotReviewer
	}

	if r.ReviewStatus != nil && *r.ReviewStatus == status {
		return r, false, nil
	}

	if err := tx.QueryRow(ctx,
		`UPDATE comment SET review_status = $2, updated_at = now() WHERE id = $1::uuid
		 RETURNING review_status, updated_at`, id, status,
	).Scan(&r.ReviewStatus, &r.UpdatedAt); err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: set review status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ReviewRequest{}, false, fmt.Errorf("comment: commit: %w", err)
	}

	return r, true, nil
}

// ReviewFields is the review request filter vocabulary. reviewer_id is a set: "reviewer_id eq
// <me>" is "assigned to me". review_status defaults to pending when no condition names it.
var ReviewFields = filter.Schema{
	"document_id":   {Expr: "c.document_id", Kind: filter.UUID},
	"field_id":      {Expr: "c.field_id", Kind: filter.UUID},
	"author_id":     {Expr: "c.author_id", Kind: filter.UUID},
	"reviewer_id":   {Expr: "ARRAY(SELECT cr.account_id FROM comment_reviewer cr WHERE cr.comment_id = c.id)", Kind: filter.UUIDSet},
	"review_status": {Expr: "c.review_status", Kind: filter.Text},
	"created_at":    {Expr: "c.created_at", Kind: filter.Time},
}

// ReviewPage is one page of review requests.
type ReviewPage struct {
	Requests   []ReviewRequest
	NextCursor string
	HasMore    bool
}

func namesField(g filter.Group, field string) bool {
	for _, c := range g.Conditions {
		if c.Field == field {
			return true
		}
	}
	for _, sub := range g.Groups {
		if namesField(sub, field) {
			return true
		}
	}

	return false
}

// ListReviews returns the review requests the caller may see, reach composed first. A filter
// that never mentions review_status gets the open ones (pending); naming it in any condition
// turns the default off.
func (s *Service) ListReviews(ctx context.Context, accountID string, p ListParams) (ReviewPage, error) {
	plan, err := ReviewFields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return ReviewPage{}, err
	}

	open := ""
	if !namesField(p.Filter, "review_status") {
		open = " AND c.review_status = 'pending'"
	}

	args := append([]any{accountID}, plan.Args...)
	limit := plan.Limit
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx,
		expand(`SELECT `+reviewColumns+`, `+strings.Join(plan.Keys, ", ")+`
		          FROM `+from+`
		         WHERE `+reachable+` AND c.review_status IS NOT NULL`+open+` AND (`+plan.Where+`) `+
			plan.Order+` LIMIT $`+strconv.Itoa(len(args)), "$1"),
		args...)
	if err != nil {
		return ReviewPage{}, fmt.Errorf("comment: list review requests: %w", err)
	}
	defer rows.Close()

	var (
		page ReviewPage
		last []*string
	)
	for rows.Next() {
		var (
			r   ReviewRequest
			key = make([]*string, len(plan.Keys))
		)
		dest := r.dest()
		for i := range key {
			dest = append(dest, &key[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return ReviewPage{}, fmt.Errorf("comment: list review requests scan: %w", err)
		}

		if len(page.Requests) == limit {
			page.HasMore = true
			break
		}
		page.Requests = append(page.Requests, r)
		last = key
	}
	if err := rows.Err(); err != nil {
		return ReviewPage{}, fmt.Errorf("comment: list review requests rows: %w", err)
	}
	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

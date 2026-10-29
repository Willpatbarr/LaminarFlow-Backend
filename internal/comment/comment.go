/*
╔═ comment.go ══════════════════════════════════════════════════════════════════════════
║  comment · plain comment threads on tickets and documents
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service            struct
║      Comment            struct
║      CreateParams       struct
║      FieldError         struct
║      ErrNotFound        error
║      ErrNotAuthor       error
║      ErrTargetNotFound  error
║      MaxBodyLength      const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  create · update · delete · list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package comment owns the comment and comment_reviewer tables.
//
// It is the write path 0017 and 0018 were waiting for: a comment hangs off exactly one
// ticket or document, and every write here indexes (or un-indexes) the comment in
// internal/search inside the same transaction, so search never disagrees with the table.
//
// Plain comments are comment.go. A review request is the same row with review_status set
// and reviewers in comment_reviewer (LAM-113); review.go writes those, and Create here
// accepts neither. Every statement scopes through the parent, not through the kind of comment.
package comment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers a comment that does not exist and one the caller cannot reach (its
// parent is archived or in another workspace), for the reason ticket.ErrNotFound does.
var ErrNotFound = errors.New("comment: not found")

// ErrNotAuthor: the comment is the caller's to see and not theirs to change. Distinct from
// ErrNotFound because the caller can already read the comment, so a 403 reveals nothing.
// A comment with no author (the account was deleted) is nobody's to change.
var ErrNotAuthor = errors.New("comment: not the author")

// ErrTargetNotFound: the ticket or document a create names is absent, archived, or outside
// the caller's workspaces.
var ErrTargetNotFound = errors.New("comment: target not found")

// MaxBodyLength is the longest a comment may be, in characters: the ticket description's limit.
const MaxBodyLength = 50000

// MaxIdempotencyKeyLength is checked by the API and repeated as a CHECK in 0040.
const MaxIdempotencyKeyLength = 128

// The reasons a create is refused as a 422. Field is the JSON name of the member at fault.
var (
	ErrTargetShape   = errors.New("exactly one of ticket_id and document_id")
	ErrFieldNeedsDoc = errors.New("field_id needs a document_id")
	ErrFieldNotOnDoc = errors.New("not a field of this document's aspect type")
	ErrRegionShape   = errors.New("region_start and region_end must be sent together")
	ErrRegionRange   = errors.New("must satisfy 0 <= region_start <= region_end")
)

// FieldError is a create refused for a reason about one member of the request.
type FieldError struct {
	Field string
	Kind  error
}

func (e *FieldError) Error() string { return "comment: " + e.Field + ": " + e.Kind.Error() }
func (e *FieldError) Unwrap() error { return e.Kind }

/*
┏━ Comment ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one comment as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID, TicketID, DocumentID    exactly one of the last two is set
┃      FieldID                     *string   document comments only
┃      RegionStart, RegionEnd      *int      both or neither
┃      AuthorID                    *string   null: the account is gone
┃      Body                        string
┃      ReviewStatus                *string   always nil for a plain comment
┃      CreatedAt, UpdatedAt        time.Time
┃      WorkspaceID                 string    for the change feed; not in the REST body
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · Update · Delete · List
*/

type Comment struct {
	ID           string
	TicketID     *string
	DocumentID   *string
	FieldID      *string
	RegionStart  *int
	RegionEnd    *int
	AuthorID     *string
	Body         string
	ReviewStatus *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// EditedAt is when the body was last edited (PUT /comments/{id}), nil if never. Unlike
	// UpdatedAt it does not move on a review status change (LAM-132).
	EditedAt *time.Time

	WorkspaceID string
}

func (c *Comment) dest() []any {
	return []any{&c.ID, &c.TicketID, &c.DocumentID, &c.FieldID, &c.RegionStart, &c.RegionEnd,
		&c.AuthorID, &c.Body, &c.ReviewStatus, &c.CreatedAt, &c.UpdatedAt, &c.EditedAt, &c.WorkspaceID}
}

const columns = `c.id::text, c.ticket_id::text, c.document_id::text, c.field_id::text,
                 c.region_start, c.region_end, c.author_id::text, c.body, c.review_status,
                 c.created_at, c.updated_at, c.edited_at, w.workspace_id::text`

// from is the comment joined to its parent and the parent's workspace, once. A comment
// reaches its workspace through whichever arm it uses (0017's exclusive arc): a ticket via
// project and team, a document directly. Both arms are live-only - a comment on an archived
// parent is hidden with it - and the caller must be a member of that workspace, so no
// statement built on this can be written without both.
const from = `comment c
	LEFT JOIN ticket t   ON t.id = c.ticket_id
	LEFT JOIN project p  ON p.id = t.project_id
	LEFT JOIN team tm    ON tm.id = p.team_id
	LEFT JOIN document d ON d.id = c.document_id
	CROSS JOIN LATERAL (SELECT COALESCE(tm.workspace_id, d.workspace_id) AS workspace_id) w`

const reachable = `(t.id IS NULL OR t.archived_at IS NULL) AND (d.id IS NULL OR d.archived_at IS NULL)
	AND EXISTS (SELECT 1 FROM workspace_member wm
	             WHERE wm.workspace_id = w.workspace_id AND wm.account_id = $ACCOUNT::uuid)`

func expand(q, accountPlaceholder string) string {
	return strings.ReplaceAll(q, "$ACCOUNT", accountPlaceholder)
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of the comment tables
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool      *pgxpool.Pool
┃      aspects   *aspect.Service    says which fields a document's type has
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                   one per process
*/

type Service struct {
	pool    *pgxpool.Pool
	aspects *aspect.Service
}

func NewService(pool *pgxpool.Pool, aspects *aspect.Service) *Service {
	return &Service{pool: pool, aspects: aspects}
}

// CreateParams is a new comment's state. Exactly one of TicketID and DocumentID is set.
type CreateParams struct {
	TicketID    *string
	DocumentID  *string
	Body        string
	FieldID     *string
	RegionStart *int
	RegionEnd   *int

	// IdempotencyKey, when set, makes the create safe to retry; see Create.
	IdempotencyKey *string
}

// validate is every rule that needs no database. Run first, so a malformed request is a 422
// whatever the caller can see.
func (p CreateParams) validate() error {
	if (p.TicketID == nil) == (p.DocumentID == nil) {
		return &FieldError{Field: "ticket_id", Kind: ErrTargetShape}
	}
	if p.FieldID != nil && p.DocumentID == nil {
		return &FieldError{Field: "field_id", Kind: ErrFieldNeedsDoc}
	}
	if (p.RegionStart == nil) != (p.RegionEnd == nil) {
		missing := "region_end"
		if p.RegionStart == nil {
			missing = "region_start"
		}

		return &FieldError{Field: missing, Kind: ErrRegionShape}
	}
	if p.RegionStart != nil && *p.RegionStart < 0 {
		return &FieldError{Field: "region_start", Kind: ErrRegionRange}
	}
	if p.RegionStart != nil && *p.RegionEnd < *p.RegionStart {
		return &FieldError{Field: "region_end", Kind: ErrRegionRange}
	}

	return textlimit.Required("body", p.Body, MaxBodyLength)
}

/*
┌─ comment ───────────────────────────────────────
│  adds a plain comment, and indexes it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string          the author, and the key's scope
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Comment
│      bool                         true: the key had been used, nothing was written
│      error                        *FieldError · *textlimit.FieldError · ErrTargetNotFound
├─ example ───────────────────────────────────────
│      key "k1" twice  →  same Comment, false then true
*/

// The key is looked up before the target is resolved: a replay returns what the first call
// made whatever the target has since become (archived, say), as a ticket replay does. The
// insert still carries ON CONFLICT, which is what makes concurrent retries race-safe - the
// lookup is only the fast path. The loser of a race reads the winner's row in a second
// statement.
//
// A replay writes nothing, so it indexes nothing; IndexComment is reached by a real insert only.
func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (Comment, bool, error) {
	if err := p.validate(); err != nil {
		return Comment{}, false, err
	}

	if p.IdempotencyKey != nil {
		if c, err := s.byKey(ctx, s.pool, accountID, *p.IdempotencyKey); err == nil {
			return c, true, nil
		} else if !errors.Is(err, ErrNotFound) {
			return Comment{}, false, err
		}
	}

	workspaceID, aspectTypeID, err := s.target(ctx, accountID, p)
	if err != nil {
		return Comment{}, false, err
	}

	// The field check is application-level because 0017 cannot do it (a composite key through
	// document.aspect_type_id's nullable column constrains nothing). A field removed between
	// this and the insert is caught by the foreign key and mapped below.
	if p.FieldID != nil {
		if err := s.checkField(ctx, workspaceID, aspectTypeID, *p.FieldID); err != nil {
			return Comment{}, false, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, false, fmt.Errorf("comment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	c := Comment{WorkspaceID: workspaceID}
	err = tx.QueryRow(ctx,
		`INSERT INTO comment (ticket_id, document_id, field_id, region_start, region_end,
		                      author_id, body, idempotency_key)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::int, $5::int, $6::uuid, $7, $8::text)
		 ON CONFLICT (author_id, idempotency_key) WHERE idempotency_key IS NOT NULL
		 DO NOTHING
		 RETURNING id::text, ticket_id::text, document_id::text, field_id::text,
		           region_start, region_end, author_id::text, body, review_status,
		           created_at, updated_at`,
		p.TicketID, p.DocumentID, p.FieldID, p.RegionStart, p.RegionEnd, accountID, p.Body, p.IdempotencyKey,
	).Scan(&c.ID, &c.TicketID, &c.DocumentID, &c.FieldID, &c.RegionStart, &c.RegionEnd,
		&c.AuthorID, &c.Body, &c.ReviewStatus, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The key was used between the lookup and the insert. Read it on this transaction's
		// connection: a pool read here would hold two connections per loser, and sixteen
		// racing retries would starve the pool. A new statement under READ COMMITTED sees
		// the winner's commit.
		if p.IdempotencyKey != nil {
			if existing, err := s.byKey(ctx, tx, accountID, *p.IdempotencyKey); err == nil {
				return existing, true, nil
			}
		}

		return Comment{}, false, fmt.Errorf("comment: insert wrote nothing")
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23503" {
			switch pg.ConstraintName {
			case "comment_field_id_fkey":
				return Comment{}, false, &FieldError{Field: "field_id", Kind: ErrFieldNotOnDoc}
			case "comment_ticket_id_fkey", "comment_document_id_fkey":
				return Comment{}, false, ErrTargetNotFound
			}
		}

		return Comment{}, false, fmt.Errorf("comment: insert: %w", err)
	}

	if err := search.IndexComment(ctx, tx, c.ID); err != nil {
		return Comment{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Comment{}, false, fmt.Errorf("comment: commit: %w", err)
	}

	return c, false, nil
}

// checkField proves fieldID is a field of the document's aspect type (nil for a normal
// document, which has none).
func (s *Service) checkField(ctx context.Context, workspaceID string, aspectTypeID *string, fieldID string) error {
	if aspectTypeID == nil {
		return &FieldError{Field: "field_id", Kind: ErrFieldNotOnDoc}
	}
	ids, err := s.aspects.FieldIDs(ctx, workspaceID, *aspectTypeID)
	if err != nil {
		return fmt.Errorf("comment: fields of type: %w", err)
	}
	for _, id := range ids {
		if strings.EqualFold(id, fieldID) {
			return nil
		}
	}

	return &FieldError{Field: "field_id", Kind: ErrFieldNotOnDoc}
}

// target proves the caller can reach the live ticket or document, and returns its workspace
// and (for a document) its aspect type.
func (s *Service) target(ctx context.Context, accountID string, p CreateParams) (workspace string, aspectType *string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT w.workspace_id::text, w.aspect_type_id::text
		   FROM (
		        SELECT tm.workspace_id, NULL::uuid AS aspect_type_id
		          FROM ticket t
		          JOIN project p ON p.id = t.project_id
		          JOIN team tm   ON tm.id = p.team_id
		         WHERE t.id = $1::uuid AND t.archived_at IS NULL
		        UNION ALL
		        SELECT d.workspace_id, d.aspect_type_id
		          FROM document d
		         WHERE d.id = $2::uuid AND d.archived_at IS NULL
		   ) w
		  WHERE EXISTS (SELECT 1 FROM workspace_member wm
		                 WHERE wm.workspace_id = w.workspace_id AND wm.account_id = $3::uuid)`,
		p.TicketID, p.DocumentID, accountID,
	).Scan(&workspace, &aspectType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrTargetNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("comment: target: %w", err)
	}

	return workspace, aspectType, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// byKey reads the caller's comment made under a key. Not scoped by reach: the caller wrote it.
func (s *Service) byKey(ctx context.Context, q querier, accountID, key string) (Comment, error) {
	var c Comment
	err := q.QueryRow(ctx,
		`SELECT `+columns+` FROM `+from+`
		  WHERE c.author_id = $1::uuid AND c.idempotency_key = $2`,
		accountID, key,
	).Scan(c.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, fmt.Errorf("comment: by key: %w", err)
	}

	return c, nil
}

/*
┌─ comment ───────────────────────────────────────
│  changes a comment's body, and reindexes it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
│      body         string
├─ out ───────────────────────────────────────────
│      Comment
│      error        ErrNotFound · ErrNotAuthor · *textlimit.FieldError
*/

// Edit changes body, updated_at and edited_at and nothing else: the target, anchor and region are fixed
// at creation. The row is locked while the author is checked, so a concurrent delete cannot
// slip between the check and the write.
func (s *Service) Update(ctx context.Context, accountID, id, body string) (Comment, error) {
	if err := textlimit.Required("body", body, MaxBodyLength); err != nil {
		return Comment{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, fmt.Errorf("comment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	c, err := s.own(ctx, tx, accountID, id)
	if err != nil {
		return Comment{}, err
	}

	err = tx.QueryRow(ctx,
		`UPDATE comment SET body = $2, updated_at = now(), edited_at = now() WHERE id = $1::uuid
		 RETURNING body, updated_at, edited_at`, id, body,
	).Scan(&c.Body, &c.UpdatedAt, &c.EditedAt)
	if err != nil {
		return Comment{}, fmt.Errorf("comment: update: %w", err)
	}

	if err := search.ClearComment(ctx, tx, c.ID); err != nil {
		return Comment{}, err
	}
	if err := search.IndexComment(ctx, tx, c.ID); err != nil {
		return Comment{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Comment{}, fmt.Errorf("comment: commit: %w", err)
	}

	return c, nil
}

/*
┌─ comment ───────────────────────────────────────
│  deletes a comment, and removes it from search
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      Comment                      as it was, for the change feed
│      error        ErrNotFound · ErrNotAuthor
*/

// Delete is a hard delete: a comment has no archive state, and comment_reviewer rows go with
// it (0018, ON DELETE CASCADE).
func (s *Service) Delete(ctx context.Context, accountID, id string) (Comment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, fmt.Errorf("comment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	c, err := s.own(ctx, tx, accountID, id)
	if err != nil {
		return Comment{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM comment WHERE id = $1::uuid`, id); err != nil {
		return Comment{}, fmt.Errorf("comment: delete: %w", err)
	}
	if err := search.ClearComment(ctx, tx, id); err != nil {
		return Comment{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Comment{}, fmt.Errorf("comment: commit: %w", err)
	}

	return c, nil
}

// own locks and returns a comment the caller can reach, and refuses unless they wrote it.
func (s *Service) own(ctx context.Context, tx pgx.Tx, accountID, id string) (Comment, error) {
	var c Comment
	err := tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM `+from+`
		         WHERE c.id = $1::uuid AND `+reachable+`
		           FOR UPDATE OF c`, "$2"),
		id, accountID,
	).Scan(c.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, ErrNotFound
	}
	if err != nil {
		return Comment{}, fmt.Errorf("comment: lock: %w", err)
	}

	if c.AuthorID == nil || *c.AuthorID != accountID {
		return Comment{}, ErrNotAuthor
	}

	return c, nil
}

// Fields is the comment filter vocabulary. A thread is read with ticket_id or document_id eq.
var Fields = filter.Schema{
	"ticket_id":   {Expr: "c.ticket_id", Kind: filter.UUID},
	"document_id": {Expr: "c.document_id", Kind: filter.UUID},
	"field_id":    {Expr: "c.field_id", Kind: filter.UUID},
	"author_id":   {Expr: "c.author_id", Kind: filter.UUID},
	"created_at":  {Expr: "c.created_at", Kind: filter.Time},
}

// Tiebreak completes every comment sort with a primary key, so the order is total.
const Tiebreak = "c.id"

// DefaultSort is oldest first: a thread reads top to bottom.
var DefaultSort = []filter.Order{{Field: "created_at"}}

// ListParams is what one page request asks for; same shape as ticket.ListParams.
type ListParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

// Page is one page of comments. No total, for the reason ticket.Page has none.
type Page struct {
	Comments   []Comment
	NextCursor string
	HasMore    bool
}

// List returns a page of the comments the caller may see: reach is composed first and no
// filter can widen it.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	plan, err := Fields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return Page{}, err
	}

	args := append([]any{accountID}, plan.Args...)
	limit := plan.Limit
	args = append(args, limit+1)

	rows, err := s.pool.Query(ctx,
		expand(`SELECT `+columns+`, `+strings.Join(plan.Keys, ", ")+`
		          FROM `+from+`
		         WHERE `+reachable+` AND (`+plan.Where+`) `+plan.Order+` LIMIT $`+strconv.Itoa(len(args)), "$1"),
		args...)
	if err != nil {
		return Page{}, fmt.Errorf("comment: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)
	for rows.Next() {
		var (
			c   Comment
			key = make([]*string, len(plan.Keys))
		)
		dest := c.dest()
		for i := range key {
			dest = append(dest, &key[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("comment: list scan: %w", err)
		}

		if len(page.Comments) == limit {
			page.HasMore = true
			break
		}
		page.Comments = append(page.Comments, c)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("comment: list rows: %w", err)
	}
	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

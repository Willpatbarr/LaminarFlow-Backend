/*
╔═ attachment.go ═══════════════════════════════════════════════════════════════════════
║  attachment · files on tickets and documents
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service            struct
║      Attachment         struct
║      CreateParams       struct
║      FieldError         struct
║      ErrNotFound        error
║      ErrTargetNotFound  error
║      ErrTooLarge        error
║      ErrTypeNotAllowed  error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  create · get · open · delete · list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package attachment owns the attachment table and the rules for what may be stored.
//
// The bytes are internal/storage's; this package decides whether a file is acceptable
// (size, type), who may reach it (anyone who can see its ticket or document), and keeps the
// row and the blob in step. Both halves of a create can fail independently, so the order is
// fixed: the blob first, the row second, and the blob removed again if the row does not
// land. A delete is the reverse: the row first, then the blob, so a failure between the two
// leaves a stray file and never a row pointing at nothing.
//
// The one thing this cannot do is notice a ticket or document being deleted outright (the
// foreign key cascades the row, not the file). Both are archived rather than deleted by the
// API, so today the orphan needs a hand-run SQL delete to produce.
package attachment

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/storage"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound covers an attachment that does not exist and one the caller cannot reach
// (its ticket or document is archived or in another workspace), indistinguishable on
// purpose.
var ErrNotFound = errors.New("attachment: not found")

// ErrTargetNotFound: the ticket or document a create names is absent, archived, or
// outside the caller's workspaces.
var ErrTargetNotFound = errors.New("attachment: target not found")

// ErrTooLarge: the file is over the configured limit. Detected while reading, so the
// rest of the stream is never consumed.
var ErrTooLarge = errors.New("attachment: file is too large")

// ErrTypeNotAllowed: the file is of a type this API does not store.
var ErrTypeNotAllowed = errors.New("attachment: content type not allowed")

// ErrTargetShape, ErrEmpty and ErrKeyShape are the reasons a create is a 422.
var (
	ErrTargetShape = errors.New("exactly one of ticket_id and document_id")
	ErrEmpty       = errors.New("the file is empty")
	ErrKeyShape    = errors.New("must be 1 to 128 printable ASCII characters")
)

// FieldError is a create refused for a reason about one member of the request.
type FieldError struct {
	Field string
	Kind  error
}

func (e *FieldError) Error() string { return "attachment: " + e.Field + ": " + e.Kind.Error() }
func (e *FieldError) Unwrap() error { return e.Kind }

/*
┏━ Attachment ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one stored file as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID, TicketID, DocumentID    exactly one of the last two is set
┃      Filename                    string   sanitised; for display and download
┃      ContentType                 string   decided by the server
┃      SizeBytes                   int64
┃      CreatedBy                   *string  null: the account is gone
┃      CreatedAt                   time.Time
┃      WorkspaceID                 string   not in the REST body
┃      StorageKey                  string   never leaves the server
*/

type Attachment struct {
	ID          string
	TicketID    *string
	DocumentID  *string
	Filename    string
	ContentType string
	SizeBytes   int64
	CreatedBy   *string
	CreatedAt   time.Time

	WorkspaceID string
	StorageKey  string
}

func (a *Attachment) dest() []any {
	return []any{&a.ID, &a.TicketID, &a.DocumentID, &a.Filename, &a.ContentType, &a.SizeBytes,
		&a.CreatedBy, &a.CreatedAt, &a.StorageKey, &a.WorkspaceID}
}

const columns = `a.id::text, a.ticket_id::text, a.document_id::text, a.filename, a.content_type,
                 a.size_bytes, a.created_by::text, a.created_at, a.storage_key, w.workspace_id::text`

// from is the attachment joined to its owner and the owner's workspace, once; the same
// arc as comment. Both arms are live-only: an attachment on an archived owner is hidden
// with it.
const from = `attachment a
	LEFT JOIN ticket t   ON t.id = a.ticket_id
	LEFT JOIN project p  ON p.id = t.project_id
	LEFT JOIN team tm    ON tm.id = p.team_id
	LEFT JOIN document d ON d.id = a.document_id
	CROSS JOIN LATERAL (SELECT COALESCE(tm.workspace_id, d.workspace_id) AS workspace_id) w`

const reachable = `(t.id IS NULL OR t.archived_at IS NULL) AND (d.id IS NULL OR d.archived_at IS NULL)
	AND EXISTS (SELECT 1 FROM workspace_member wm
	             WHERE wm.workspace_id = w.workspace_id AND wm.account_id = $ACCOUNT::uuid)`

func expand(q, accountPlaceholder string) string {
	return strings.ReplaceAll(q, "$ACCOUNT", accountPlaceholder)
}

// sniffLen is how much of the file is read to decide its type: what
// net/http.DetectContentType looks at.
const sniffLen = 512

// MaxIdempotencyKeyLength is checked by the API and repeated as a CHECK in 0044.
const MaxIdempotencyKeyLength = 128

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of the attachment table
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool      *pgxpool.Pool
┃      store     storage.Storage
┃      max       int64            the per-file limit, in bytes
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                 one per process
*/

type Service struct {
	pool  *pgxpool.Pool
	store storage.Storage
	max   int64
}

func NewService(pool *pgxpool.Pool, store storage.Storage, maxBytes int64) *Service {
	return &Service{pool: pool, store: store, max: maxBytes}
}

// MaxBytes is the per-file limit, for the 413 body and the docs.
func (s *Service) MaxBytes() int64 { return s.max }

// CreateParams is a new attachment. Exactly one of TicketID and DocumentID is set.
type CreateParams struct {
	TicketID   *string
	DocumentID *string

	// Filename and DeclaredType are what the client said. Neither is trusted: the name is
	// sanitised and the type is a hint to the sniffer.
	Filename     string
	DeclaredType string

	// File is read once, as a stream.
	File io.Reader

	// IdempotencyKey, when set, makes the create safe to retry; see Create.
	IdempotencyKey *string
}

func (p CreateParams) validate() error {
	if (p.TicketID == nil) == (p.DocumentID == nil) {
		return &FieldError{Field: "ticket_id", Kind: ErrTargetShape}
	}
	if p.IdempotencyKey != nil && (len(*p.IdempotencyKey) < 1 || len(*p.IdempotencyKey) > MaxIdempotencyKeyLength || !printable(*p.IdempotencyKey)) {
		return &FieldError{Field: "Idempotency-Key", Kind: ErrKeyShape}
	}

	return nil
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}

	return true
}

/*
┌─ attachment ────────────────────────────────────
│  stores a file and records it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string          the uploader, and the key's scope
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Attachment
│      bool                         true: the key had been used, nothing was stored
│      error        *FieldError · ErrTargetNotFound · ErrTooLarge · ErrTypeNotAllowed
├─ example ───────────────────────────────────────
│      key "k1" twice  →  same Attachment, false then true
*/

// The order is cheapest and most revealing first, and nothing is written until the last
// refusal that does not need the bytes has passed: shape, then the key (a replay returns
// what the first call made and never reads the body), then the owner (so a caller cannot
// make the server store a file against something they cannot see), then the type from the
// first 512 bytes, then the stream. The size is enforced while reading, by a reader that
// fails the moment it has handed over one byte more than the limit.
//
// The row is inserted with ON CONFLICT on the key, which is what makes concurrent retries
// safe; the lookup before it is only the fast path. The loser of a race removes the blob it
// wrote and answers with the winner's row.
func (s *Service) Create(ctx context.Context, accountID string, p CreateParams) (Attachment, bool, error) {
	if err := p.validate(); err != nil {
		return Attachment{}, false, err
	}

	if p.IdempotencyKey != nil {
		if a, err := s.byKey(ctx, s.pool, accountID, *p.IdempotencyKey); err == nil {
			return a, true, nil
		} else if !errors.Is(err, ErrNotFound) {
			return Attachment{}, false, err
		}
	}

	workspaceID, err := s.target(ctx, accountID, p)
	if err != nil {
		return Attachment{}, false, err
	}

	br := bufio.NewReaderSize(p.File, sniffLen)
	head, err := br.Peek(sniffLen)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return Attachment{}, false, fmt.Errorf("attachment: read: %w", err)
	}
	if len(head) == 0 {
		return Attachment{}, false, &FieldError{Field: "file", Kind: ErrEmpty}
	}

	contentType, err := resolveType(p.DeclaredType, head)
	if err != nil {
		return Attachment{}, false, err
	}

	key, err := newKey()
	if err != nil {
		return Attachment{}, false, err
	}
	size, err := s.store.Put(ctx, key, &limitReader{r: br, left: s.max})
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return Attachment{}, false, ErrTooLarge
		}

		return Attachment{}, false, fmt.Errorf("attachment: store: %w", err)
	}

	a := Attachment{WorkspaceID: workspaceID, StorageKey: key}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO attachment (ticket_id, document_id, filename, content_type, size_bytes,
		                         storage_key, created_by, idempotency_key)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::uuid, $8::text)
		 ON CONFLICT (created_by, idempotency_key) WHERE idempotency_key IS NOT NULL
		 DO NOTHING
		 RETURNING id::text, ticket_id::text, document_id::text, filename, content_type,
		           size_bytes, created_by::text, created_at`,
		p.TicketID, p.DocumentID, SanitizeFilename(p.Filename), contentType, size, key, accountID, p.IdempotencyKey,
	).Scan(&a.ID, &a.TicketID, &a.DocumentID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.CreatedBy, &a.CreatedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key was used between the lookup and the insert: the blob just written is
		// surplus.
		s.discard(key)
		if p.IdempotencyKey != nil {
			if existing, err := s.byKey(ctx, s.pool, accountID, *p.IdempotencyKey); err == nil {
				return existing, true, nil
			}
		}

		return Attachment{}, false, fmt.Errorf("attachment: insert wrote nothing")
	case err != nil:
		s.discard(key)

		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23503" {
			// The owner went away between the check and the insert.
			return Attachment{}, false, ErrTargetNotFound
		}

		return Attachment{}, false, fmt.Errorf("attachment: insert: %w", err)
	}

	return a, false, nil
}

// discard removes a blob no row refers to. Best effort and logged: the request has already
// failed or been answered, and a stray file is the mild outcome.
func (s *Service) discard(key string) {
	// Not the request's context: a cancelled request is exactly when this matters.
	if err := s.store.Delete(context.WithoutCancel(context.Background()), key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Printf("attachment: remove unreferenced blob %s: %v", key, err)
	}
}

// limitReader fails with ErrTooLarge as soon as the underlying stream yields more than
// left bytes, so an oversize upload is refused after limit+1 bytes, not after the whole
// of it has been read.
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	// One byte of headroom, to tell "exactly at the limit" from "over it".
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}

	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, ErrTooLarge
	}

	return n, err
}

// target proves the caller can reach the live ticket or document, and returns its workspace.
func (s *Service) target(ctx context.Context, accountID string, p CreateParams) (string, error) {
	var workspace string
	err := s.pool.QueryRow(ctx,
		`SELECT w.workspace_id::text
		   FROM (
		        SELECT tm.workspace_id
		          FROM ticket t
		          JOIN project p ON p.id = t.project_id
		          JOIN team tm   ON tm.id = p.team_id
		         WHERE t.id = $1::uuid AND t.archived_at IS NULL
		        UNION ALL
		        SELECT d.workspace_id
		          FROM document d
		         WHERE d.id = $2::uuid AND d.archived_at IS NULL
		   ) w
		  WHERE EXISTS (SELECT 1 FROM workspace_member wm
		                 WHERE wm.workspace_id = w.workspace_id AND wm.account_id = $3::uuid)`,
		p.TicketID, p.DocumentID, accountID,
	).Scan(&workspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTargetNotFound
	}
	if err != nil {
		return "", fmt.Errorf("attachment: target: %w", err)
	}

	return workspace, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// byKey reads the caller's attachment made under a key. Not scoped by reach: the caller
// made it.
func (s *Service) byKey(ctx context.Context, q querier, accountID, key string) (Attachment, error) {
	var a Attachment
	err := q.QueryRow(ctx,
		`SELECT `+columns+` FROM `+from+`
		  WHERE a.created_by = $1::uuid AND a.idempotency_key = $2`,
		accountID, key,
	).Scan(a.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("attachment: by key: %w", err)
	}

	return a, nil
}

// Get returns an attachment the caller can reach.
func (s *Service) Get(ctx context.Context, accountID, id string) (Attachment, error) {
	var a Attachment
	err := s.pool.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM `+from+`
		         WHERE a.id = $1::uuid AND `+reachable, "$2"),
		id, accountID,
	).Scan(a.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("attachment: get: %w", err)
	}

	return a, nil
}

// Open returns the bytes of an attachment Get returned. A blob that is missing from storage
// while its row exists is a real fault (a restored database without its files), and comes
// back as storage.ErrNotFound for the caller to report as such.
func (s *Service) Open(ctx context.Context, a Attachment) (io.ReadCloser, error) {
	return s.store.Open(ctx, a.StorageKey)
}

/*
┌─ attachment ────────────────────────────────────
│  deletes the row, then the blob
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      id           string
├─ out ───────────────────────────────────────────
│      Attachment                   as it was
│      error        ErrNotFound
*/

// Any member who can reach the owner may delete its attachments, as any member may edit the
// ticket or document itself; unlike a comment, a file is not someone's words.
//
// The row goes first and the blob after. A blob already missing is not an error: the aim of
// the call is that the file is gone. Any other blob failure is logged and the delete still
// succeeds - the row is the source of truth, and failing here would leave an attachment the
// user cannot get rid of over a stray file they cannot see.
func (s *Service) Delete(ctx context.Context, accountID, id string) (Attachment, error) {
	a, err := s.Get(ctx, accountID, id)
	if err != nil {
		return Attachment{}, err
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM attachment WHERE id = $1::uuid`, id)
	if err != nil {
		return Attachment{}, fmt.Errorf("attachment: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Deleted by someone else between the read and here.
		return Attachment{}, ErrNotFound
	}

	if err := s.store.Delete(ctx, a.StorageKey); err != nil && !errors.Is(err, storage.ErrNotFound) {
		log.Printf("attachment: row %s deleted but blob %s was not removed: %v", a.ID, a.StorageKey, err)
	}

	return a, nil
}

// Fields is the attachment filter vocabulary. An owner's files are read with ticket_id or
// document_id eq.
var Fields = filter.Schema{
	"ticket_id":    {Expr: "a.ticket_id", Kind: filter.UUID},
	"document_id":  {Expr: "a.document_id", Kind: filter.UUID},
	"created_by":   {Expr: "a.created_by", Kind: filter.UUID},
	"content_type": {Expr: "a.content_type", Kind: filter.Text},
	"created_at":   {Expr: "a.created_at", Kind: filter.Time},
}

// Tiebreak completes every attachment sort with a primary key, so the order is total.
const Tiebreak = "a.id"

// DefaultSort is oldest first: attachments read in the order they were added.
var DefaultSort = []filter.Order{{Field: "created_at"}}

// ListParams is what one page request asks for; same shape as comment.ListParams.
type ListParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

// Page is one page of attachments. No total, for the reason ticket.Page has none.
type Page struct {
	Attachments []Attachment
	NextCursor  string
	HasMore     bool
}

// List returns a page of the attachments the caller may see: reach is composed first and
// no filter can widen it.
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
		return Page{}, fmt.Errorf("attachment: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)
	for rows.Next() {
		var (
			a   Attachment
			key = make([]*string, len(plan.Keys))
		)
		dest := a.dest()
		for i := range key {
			dest = append(dest, &key[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("attachment: list scan: %w", err)
		}

		if len(page.Attachments) == limit {
			page.HasMore = true
			break
		}
		page.Attachments = append(page.Attachments, a)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("attachment: list rows: %w", err)
	}
	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

// newKey is a random (version 4) uuid. It is the whole of a blob's name: nothing the
// client sent is in it.
func newKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("attachment: key: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

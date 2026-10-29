/*
╔═ activity.go ═════════════════════════════════════════════════════════════════════════
║  activity · the history of tickets and documents
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Entry          struct
║      Tx             interface
║      Record         func
║      Service        struct
║      Scope          func
║      Fields         filter.Schema
║      Clip           func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket    →  Create · Update · Move · Archive · Unarchive · MoveStatus
║      internal/document  →  Write · Archive · Unarchive
║      internal/api       →  POST /api/v1/activity/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package activity owns the activity table (LAM-89).
//
// A row says that something meaningful happened to a ticket or a document, who did it and
// when. The writers are internal/ticket and internal/document, which call Record inside the
// transaction of the change itself, so a rolled-back change leaves no row and a committed
// one always has its row. Nothing else writes the table (boundary_test.go).
//
// Coarse by design. Tickets get field-level entries; documents get a handful of kinds
// (created, title changed, type converted, field set changed, archived, unarchived). A body
// save is not recorded: version history is the answer to "what did the text say".
package activity

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The closed set of kinds, repeated as a CHECK in 0043.
const (
	Created           = "created"
	StatusChanged     = "status_changed"
	AssigneeChanged   = "assignee_changed"
	ParentChanged     = "parent_changed"
	LabelAdded        = "label_added"
	LabelRemoved      = "label_removed"
	Archived          = "archived"
	Unarchived        = "unarchived"
	AspectTypeChanged = "aspect_type_changed"
	FieldChanged      = "field_changed"
)

// The two entity types the API names.
const (
	EntityTicket   = "ticket"
	EntityDocument = "document"
)

// maxSummaryChars is the longest text summary stored, in characters.
const maxSummaryChars = 200

/*
┏━ Entry ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one row of the log
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID, CreatedAt    set by Record
┃      TicketID         *string    exactly one of the two is set
┃      DocumentID       *string
┃      ActorID          *string    null: no account, or the account is gone
┃      Kind             string     one of the constants above
┃      Field            *string    field_changed only
┃      Before, After    *string    summary_before / summary_after
┃      BeforeName,      *string    status_changed only: the status names at the time
┃      AfterName
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      the writers; Record fills ID and CreatedAt
*/
type Entry struct {
	ID         string
	TicketID   *string
	DocumentID *string
	ActorID    *string
	Kind       string
	Field      *string
	Before     *string
	After      *string
	BeforeName *string
	AfterName  *string
	CreatedAt  time.Time
}

// EntityType says whether the entry is about a ticket or a document.
func (e Entry) EntityType() string {
	if e.TicketID != nil {
		return EntityTicket
	}

	return EntityDocument
}

// EntityID is the id of the ticket or document the entry is about.
func (e Entry) EntityID() string {
	if e.TicketID != nil {
		return *e.TicketID
	}

	return *e.DocumentID
}

// Tx is whatever can run a statement inside the writer's transaction.
type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

/*
┌─ activity ──────────────────────────────────────
│  inserts one entry in the caller's transaction
├─ in ────────────────────────────────────────────
│      ctx    context.Context
│      tx     Tx           the write's own transaction
│      e      Entry        ID and CreatedAt ignored
├─ out ───────────────────────────────────────────
│      Entry                as stored
│      error
*/

// Record is the only INSERT into activity. It takes the transaction rather than a pool
// because that is the whole point: the entry commits with the change or not at all.
func Record(ctx context.Context, tx Tx, e Entry) (Entry, error) {
	err := tx.QueryRow(ctx,
		`INSERT INTO activity (ticket_id, document_id, actor_id, kind, field, summary_before, summary_after, name_before, name_after)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9)
		 RETURNING id::text, created_at`,
		e.TicketID, e.DocumentID, e.ActorID, e.Kind, e.Field, e.Before, e.After, e.BeforeName, e.AfterName,
	).Scan(&e.ID, &e.CreatedAt)
	if err != nil {
		return Entry{}, fmt.Errorf("activity: record %s: %w", e.Kind, err)
	}

	return e, nil
}

// Clip cuts text to a summary: at most 200 characters, counted as characters and not bytes,
// with an ellipsis when it was cut.
func Clip(s string) string {
	if utf8.RuneCountInString(s) <= maxSummaryChars {
		return s
	}

	return string([]rune(s)[:maxSummaryChars-1]) + "…"
}

// Ptr returns &s, or nil for "". The writers use it for optional ids.
func Ptr(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// Service reads the log. Writing is Record, from the services whose changes it describes.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service reading through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Fields is the filter vocabulary. entity_type and entity_id are required, as top-level
// eq conditions of an and filter (Scope); kind, actor_id and created_at narrow the history
// further.
var Fields = filter.Schema{
	"entity_type": {Expr: "(CASE WHEN a.ticket_id IS NOT NULL THEN 'ticket' ELSE 'document' END)", Kind: filter.Text},
	"entity_id":   {Expr: "COALESCE(a.ticket_id, a.document_id)", Kind: filter.UUID},
	"kind":        {Expr: "a.kind", Kind: filter.Text},
	"actor_id":    {Expr: "a.actor_id", Kind: filter.UUID},
	"created_at":  {Expr: "a.created_at", Kind: filter.Time},
}

// Tiebreak completes every sort with a primary key, so the order is total.
const Tiebreak = "a.id"

// DefaultSort is newest first: a history reads from the latest change down.
var DefaultSort = []filter.Order{{Field: "created_at", Desc: true}}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

/*
┌─ activity ──────────────────────────────────────
│  the entity a list request is about
├─ in ────────────────────────────────────────────
│      g    filter.Group
├─ out ───────────────────────────────────────────
│      entityType    string    ticket · document
│      entityID      string
│      error         *filter.Error, one fault per missing or bad member
├─ example ───────────────────────────────────────
│      and[entity_type eq ticket, entity_id eq X]  →  "ticket", X
*/

// Scope insists on both members as top-level eq conditions of an and group (an or, or a
// condition nested in a group, could widen the list past one entity), and says where each
// one is wrong: body.filter.entity_type, body.filter.entity_id.
func Scope(g filter.Group) (entityType, entityID string, err error) {
	var faults []filter.Fault

	find := func(name string) (filter.Condition, bool) {
		if g.Op == filter.Or && len(g.Conditions)+len(g.Groups) > 1 {
			return filter.Condition{}, false
		}
		for _, c := range g.Conditions {
			if c.Field == name && c.Op == filter.Eq {
				return c, true
			}
		}

		return filter.Condition{}, false
	}

	if c, ok := find("entity_type"); !ok {
		faults = append(faults, filter.Fault{Location: "body.filter.entity_type",
			Message: "required: an eq condition on entity_type (ticket or document) at the top level of an and filter"})
	} else if s, _ := c.Value.(string); s != EntityTicket && s != EntityDocument {
		faults = append(faults, filter.Fault{Location: "body.filter.entity_type",
			Message: "must be ticket or document", Value: c.Value})
	} else {
		entityType = s
	}

	if c, ok := find("entity_id"); !ok {
		faults = append(faults, filter.Fault{Location: "body.filter.entity_id",
			Message: "required: an eq condition on entity_id (a uuid) at the top level of an and filter"})
	} else if s, _ := c.Value.(string); !uuidRE.MatchString(s) {
		faults = append(faults, filter.Fault{Location: "body.filter.entity_id",
			Message: "must be a uuid", Value: c.Value})
	} else {
		entityID = strings.ToLower(s)
	}

	if len(faults) > 0 {
		return "", "", &filter.Error{Faults: faults}
	}

	return entityType, entityID, nil
}

// ListParams is one page request, same shape as comment.ListParams.
type ListParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

// Page is one page of entries. No total, as everywhere.
type Page struct {
	Entries    []Entry
	NextCursor string
	HasMore    bool
}

// List returns a page of one entity's history, newest first.
//
// It does not check that the caller may see the entity: it cannot read ticket or document
// (sqlguard), so internal/api proves reach through those services first and a caller who
// cannot see the entity never gets here. What this adds to the filter is the explicit
// ticket_id / document_id predicate that lets Postgres use the entity's partial index; the
// entity_* conditions in the filter say the same thing again.
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	entityType, entityID, err := Scope(p.Filter)
	if err != nil {
		return Page{}, err
	}

	plan, err := Fields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return Page{}, err
	}

	column := "a.ticket_id"
	if entityType == EntityDocument {
		column = "a.document_id"
	}

	args := append([]any{entityID}, plan.Args...)
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx,
		`SELECT a.id::text, a.ticket_id::text, a.document_id::text, a.actor_id::text, a.kind,
		        a.field, a.summary_before, a.summary_after, a.name_before, a.name_after, a.created_at, `+strings.Join(plan.Keys, ", ")+`
		   FROM activity a
		  WHERE `+column+` = $1::uuid AND (`+plan.Where+`) `+plan.Order+` LIMIT $`+strconv.Itoa(len(args)),
		args...)
	if err != nil {
		return Page{}, fmt.Errorf("activity: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)
	for rows.Next() {
		var (
			e   Entry
			key = make([]*string, len(plan.Keys))
		)
		dest := []any{&e.ID, &e.TicketID, &e.DocumentID, &e.ActorID, &e.Kind,
			&e.Field, &e.Before, &e.After, &e.BeforeName, &e.AfterName, &e.CreatedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("activity: list scan: %w", err)
		}

		if len(page.Entries) == plan.Limit {
			page.HasMore = true
			break
		}
		page.Entries = append(page.Entries, e)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("activity: list rows: %w", err)
	}
	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

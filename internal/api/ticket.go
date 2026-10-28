/*
╔═ ticket.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · ticket resource
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeTicketNotFound    const
║      CodeTicketConflict    const
║      CodeTicketArchived    const
║      ticketConflict        struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  create · get · list · update · archive · unarchive
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
)

// One 404 for absent, archived, and not-yours alike - the service collapses them on
// purpose, and a distinct code here would undo that by telling a caller which it hit.
const CodeTicketNotFound = "ticket_not_found"

// The two 409s a write carrying If-Match can get, and they are two codes because a
// client does opposite things with them. ticket_conflict: someone edited it, show both
// versions and let the person choose. ticket_archived: someone removed it, and the
// choices are to discard the local edit, or to keep it as a new ticket. Both carry the
// server's current copy, who changed it, and when - see ticketConflict.
const (
	CodeTicketConflict = "ticket_conflict"
	CodeTicketArchived = "ticket_archived"
)

// The two 422s a ticket write can get for an id it names. Each is its own code because
// each has its own field to fix, and a form puts the message beside that input. Both
// collapse their causes on purpose: status_not_found is "no such status" and "a status
// of another team" alike, assignee_not_found is "no such account" and "an account
// outside this workspace" alike. A caller that could tell them apart could probe for ids
// it is not entitled to know exist.
const (
	CodeStatusNotFound   = "status_not_found"
	CodeAssigneeNotFound = "assignee_not_found"

	// CodeNeighbourNotFound is a move's before_id or after_id that is not a live ticket
	// in the target column (LAM-85).
	CodeNeighbourNotFound = "neighbour_not_found"
)

// This file is the template the other five resources copy, so its shape matters as
// much as its behaviour: one input struct per verb, handlers that parse and format and
// nothing else, every operation marked RequireAuth, and every service call passed the
// caller's account.

type ticketBody struct {
	ID          string  `json:"id"`
	ProjectID   string  `json:"project_id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	StatusID    *string `json:"status_id" doc:"Null is normal - no status is sacred, so a ticket may have none."`
	AssigneeID  *string `json:"assignee_id" doc:"Null is normal - an unassigned ticket."`

	EpicID      *string  `json:"epic_id" doc:"The ticket's epic. Null is normal - the hierarchy is flexible and a ticket needs no parent."`
	MilestoneID *string  `json:"milestone_id" doc:"The ticket's milestone. Null is normal."`
	LabelIDs    []string `json:"label_ids" doc:"The ticket's labels, in id order. Never null; empty for none."`

	UpdatedAt time.Time `json:"updated_at" doc:"When the ticket last changed - an edit, an archive or a restore. For display; to compare versions use version."`
	UpdatedBy *string   `json:"updated_by" doc:"The account that made the last change. Null when it was not recorded (tickets from before this field existed) or that account has since been deleted - show it as someone."`
	Position  int       `json:"position" doc:"Order within a board column: sort by position, then id. A hint, not a slot - duplicates are legal. Written by POST /tickets/{id}/move; a new ticket goes to the end of its project."`
	Version   int       `json:"version" minimum:"1" doc:"Starts at 1 and rises by exactly 1 on every update, archive and restore. Send it back as If-Match to make a write conditional; also in the ETag header. See the If-Match parameter."`
}

// linkedDocumentBody is a document as a ticket lists it (LAM-111): a row's worth, not the body.
type linkedDocumentBody struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Type         string     `json:"type" enum:"normal,aspect"`
	AspectTypeID *string    `json:"aspect_type_id" doc:"Set exactly when type is aspect; null otherwise."`
	ArchivedAt   *time.Time `json:"archived_at" doc:"Null while live. A link survives an archive, so an archived document is listed with this set."`
}

// ticketDetailBody is what GET /tickets/{id} returns: the ticket, plus the documents linked
// to it (LAM-111). Only that read carries them - lists, writes and change-feed events keep
// ticketBody, because a link is neither the ticket's version nor cheap to fetch for a page of
// rows - so a client learns of a changed link from ticket.document_linked and _unlinked.
// TicketBody is ticketBody under an exported name, only so it can be embedded: huma skips
// unexported embedded fields (see Preconditioned), and the detail would lose every ticket
// member. An alias, so the schema and the members are exactly ticketBody's.
type TicketBody = ticketBody

type ticketDetailBody struct {
	TicketBody
	Documents []linkedDocumentBody `json:"documents" doc:"The documents linked to this ticket, oldest link first, archived ones included. Never null; empty for none."`
}

type ticketDetailOutput struct {
	ETag string `header:"ETag" doc:"The ticket's version in quotes, as If-Match takes it back."`
	Body ticketDetailBody
}

func asLinkedDocuments(docs []ticket.LinkedDocument) []linkedDocumentBody {
	out := make([]linkedDocumentBody, len(docs))
	for i, d := range docs {
		out[i] = linkedDocumentBody{ID: d.ID, Title: d.Title, Type: d.Type, AspectTypeID: d.AspectTypeID, ArchivedAt: utcOrNil(d.ArchivedAt)}
	}

	return out
}

// linkNotFound is the one 404 of the link operations. Unlike the rest of the ticket API they
// answer not_found, not ticket_not_found: the missing thing may be the ticket or the document,
// and a code naming one would tell the caller which, which for a foreign id is a probe.
func linkNotFound(err error) error {
	if errors.Is(err, ticket.ErrNotFound) {
		return NewError(http.StatusNotFound, CodeNotFound, "no such ticket or document")
	}

	return err
}

type linkDocumentInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		DocumentID string `json:"document_id" required:"true" format:"uuid" doc:"The document to link. Unknown, or in another workspace than the ticket: 404 not_found. Archived is fine."`
	}
}

type unlinkDocumentInput struct {
	ID         string `path:"id" format:"uuid"`
	DocumentID string `path:"document_id" format:"uuid"`
}

type ticketOutput struct {
	ETag string `header:"ETag" doc:"The ticket's version in quotes, as If-Match takes it back."`
	Body ticketBody
}

// versionOutput is the answer to a write that has no body. DELETE and unarchive stay
// 204 - v1 is additive-only - but the client that just archived at version 4 is holding
// a ticket now at 5, and a read cannot tell it (an archived ticket reads as not found).
// The header can.
type versionOutput struct {
	ETag string `header:"ETag" doc:"The ticket's new version in quotes, as If-Match takes it back."`
}

// Preconditioned is the optional precondition, shared by every write that takes one so
// the format is spelled out once.
//
// A header rather than a body field, for three reasons. DELETE and unarchive have no
// body to put it in, and a precondition that PUT takes in one place and DELETE in
// another is two rules. If-Match is what HTTP already calls this - RFC 9110 section
// 13.1.1 - so a client library or a proxy that knows the header does the right thing
// unprompted. And a request with no header is exactly the request that existed before,
// so an API-token script that has never heard of versions is untouched.
//
// The value is the strong entity-tag this API emits as ETag: the version in double
// quotes. Anything else is refused as a validation error rather than guessed at, since
// guessing at a precondition is how a write goes through that should not have. A list
// of tags, a weak tag, and * are not supported.
// Exported, though nothing outside this package uses it: huma skips unexported
// fields, and an embedded struct's exportedness is its type name's.
type Preconditioned struct {
	IfMatch string `header:"If-Match" pattern:"^\"[1-9][0-9]{0,8}\"$" example:"\"4\"" doc:"Make the write conditional on the ticket still being at the version you last saw, written as the ETag is: the number in double quotes, for example \"4\". If the ticket has moved on the write is refused with a 409 and nothing changes: ticket_conflict if it was edited, ticket_archived if it was archived - each carrying the current copy, who changed it and when. Omit it for a plain last-write-wins write."`
}

// ifVersion reads the precondition, nil when there is none.
func (p Preconditioned) ifVersion() (*int, error) {
	if p.IfMatch == "" {
		return nil, nil
	}

	// The pattern has already vouched for the shape, so this cannot fail; it reports
	// rather than ignores in case someone loosens the pattern.
	n, err := strconv.Atoi(strings.Trim(p.IfMatch, `"`))
	if err != nil || n < 1 {
		return nil, NewError(http.StatusUnprocessableEntity, defaultCode(http.StatusUnprocessableEntity),
			"If-Match must be a version in double quotes, like \"4\"")
	}

	return &n, nil
}

func etag(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

// idempotencyHeader is spelled once. Go canonicalises header names, and huma's tag and
// the resolver below must agree on which one they mean.
const idempotencyHeader = "Idempotency-Key"

type createTicketInput struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" pattern:"^[ -~]+$" example:"2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47" doc:"Makes the create safe to retry. The same key from the same account returns the ticket the first call created instead of creating another - the original's current state, with Idempotent-Replayed: true. 1 to 128 printable ASCII characters; a UUID is ideal. Keys are per account and never expire. A replay ignores the body it carries: send a new key for a different ticket. Omit it for a plain create."`
	Body           struct {
		ProjectID   string  `json:"project_id" required:"true" format:"uuid"`
		Title       string  `json:"title" required:"true" minLength:"1" maxLength:"200" doc:"1 to 200 characters (not bytes), and not only whitespace."`
		Description string  `json:"description,omitempty" maxLength:"50000" doc:"Up to 50,000 characters (not bytes). Omit for none."`
		StatusID    *string `json:"status_id,omitempty" format:"uuid" nullable:"true" doc:"Must be a status of the project's team, or status_not_found. Omit, or null, for no status."`
		AssigneeID  *string `json:"assignee_id,omitempty" format:"uuid" nullable:"true" doc:"Must be a member of the project's workspace, or assignee_not_found. Omit, or null, for unassigned."`

		EpicID      *string  `json:"epic_id,omitempty" format:"uuid" nullable:"true" doc:"Must be an epic of the ticket's project, or a 422 epic_not_found. Omit, or null, for none."`
		MilestoneID *string  `json:"milestone_id,omitempty" format:"uuid" nullable:"true" doc:"Must be a milestone of the ticket's project, or a 422 milestone_not_found. Omit, or null, for none."`
		LabelIDs    []string `json:"label_ids,omitempty" doc:"Labels of the project's team. An id that is not one is a 422 label_not_found at body.label_ids[i]. Omit for none."`
	}
}

// Resolve catches the two shapes of the header huma cannot see. It treats an empty
// value as absent, which is right for most parameters and wrong here: a client that
// sends Idempotency-Key with nothing in it believes the create is protected, and
// silently treating it as unprotected is how a retry makes a duplicate. And a header
// sent twice is ambiguous - the server would use the first and the client may have meant
// the second.
//
// Everything else about the key's shape (printable ASCII, the length) is in the tag, so
// it is also in the OpenAPI document.
func (in *createTicketInput) Resolve(ctx huma.Context) []error {
	var values []string
	ctx.EachHeader(func(name, value string) {
		if strings.EqualFold(name, idempotencyHeader) {
			values = append(values, value)
		}
	})

	switch {
	case len(values) > 1:
		return []error{&huma.ErrorDetail{Location: "header." + idempotencyHeader,
			Message: "send the Idempotency-Key header once"}}
	case len(values) == 1 && values[0] == "":
		return []error{&huma.ErrorDetail{Location: "header." + idempotencyHeader,
			Message: "must be 1 to 128 printable ASCII characters, or omitted"}}
	}

	return nil
}

// key is the header as the service takes it: nil when none was sent.
func (in *createTicketInput) key() *string {
	if in.IdempotencyKey == "" {
		return nil
	}

	return &in.IdempotencyKey
}

// createTicketOutput is ticketOutput plus whether the response is a replay. The status
// is 200 for both - a fresh create has always answered 200, and v1 is additive - so the
// header is how a client tells them apart.
type createTicketOutput struct {
	ETag     string `header:"ETag" doc:"The ticket's version in quotes, as If-Match takes it back."`
	Replayed string `header:"Idempotent-Replayed" enum:"true,false" doc:"Only when Idempotency-Key was sent. true: that key had been used, nothing was created, and the body is the original ticket as it is now. false: this request created the ticket."`
	Body     ticketBody
}

type getTicketInput struct {
	ID string `path:"id" format:"uuid"`
}

// writeTicketInput is the input of the two writes that have no body, archive and
// unarchive.
type writeTicketInput struct {
	ID string `path:"id" format:"uuid"`
	Preconditioned
}

type updateTicketInput struct {
	ID string `path:"id" format:"uuid"`
	Preconditioned
	Body struct {
		Title       string  `json:"title" required:"true" minLength:"1" maxLength:"200" doc:"1 to 200 characters (not bytes), and not only whitespace."`
		Description string  `json:"description" maxLength:"50000" doc:"Up to 50,000 characters (not bytes)."`
		StatusID    *string `json:"status_id" format:"uuid" doc:"Must be a status of the ticket's project's team, or status_not_found. Null clears it."`
		AssigneeID  *string `json:"assignee_id" format:"uuid" doc:"Must be a member of the ticket's workspace, or assignee_not_found. Null clears it."`

		// Unlike status_id and assignee_id these three are optional, and leave the ticket alone
		// when omitted: a client written before the hierarchy existed must not clear it by
		// editing a title. See patchValue.
		EpicID      patchUUID `json:"epic_id,omitempty" doc:"Must be an epic of the ticket's project, or a 422 epic_not_found. Omit to leave it, null to clear it."`
		MilestoneID patchUUID `json:"milestone_id,omitempty" doc:"Must be a milestone of the ticket's project, or a 422 milestone_not_found. Omit to leave it, null to clear it."`
		LabelIDs    []string  `json:"label_ids,omitempty" doc:"The complete set of labels, all of them labels of the ticket's project's team; an id that is not one is a 422 label_not_found at body.label_ids[i]. Omit to leave the labels alone; [] clears them."`
	}
}

// moveTicketInput is a drop: the column (its status) and where in it. At most one of
// position, before_id and after_id; none means the end of the column.
type moveTicketInput struct {
	ID string `path:"id" format:"uuid"`
	Preconditioned
	Body struct {
		StatusID string  `json:"status_id" required:"true" format:"uuid" doc:"The column the ticket lands in, as its status. Must be a status of the ticket's project's team, or status_not_found."`
		Position *int    `json:"position,omitempty" minimum:"0" doc:"Written as given. Prefer before_id or after_id, which pick a position between neighbours."`
		BeforeID *string `json:"before_id,omitempty" format:"uuid" doc:"Land just above this ticket, which must be a live ticket in the target column."`
		AfterID  *string `json:"after_id,omitempty" format:"uuid" doc:"Land just below this ticket, which must be a live ticket in the target column."`
	}
}

func asBody(t ticket.Ticket) ticketBody {
	return ticketBody{
		ID:          t.ID,
		ProjectID:   t.ProjectID,
		Title:       t.Title,
		Description: t.Description,
		StatusID:    t.StatusID,
		AssigneeID:  t.AssigneeID,
		EpicID:      t.EpicID,
		MilestoneID: t.MilestoneID,
		LabelIDs:    labelIDs(t.LabelIDs),
		UpdatedAt:   t.UpdatedAt.UTC(),
		UpdatedBy:   t.UpdatedBy,
		Position:    t.Position,
		Version:     t.Version,
	}
}

// labelIDs is never nil, so the JSON is [] and not null.
func labelIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}

	return ids
}

func asOutput(t ticket.Ticket) *ticketOutput {
	return &ticketOutput{ETag: etag(t.Version), Body: asBody(t)}
}

/*
┏━ ticketConflict ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the 409 body: the envelope, plus what the client needs to resolve it
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      huma.StatusError
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ErrorModel    huma.ErrorModel    RFC 9457, embedded and inlined
┃      Code          string             ticket_conflict · ticket_archived
┃      Current       ticketBody         the server's copy, archived or not
┃      ChangedBy     *changedBy         null when not recorded
┃      ChangedAt     time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      conflictProblem                  one per refused write
*/

// ticketConflict extends the envelope with RFC 9457 extension members, which is the
// standard's own mechanism for exactly this. It embeds huma.ErrorModel rather than
// Error for a Go reason: a field named Error would shadow the Error() method promoted
// from the model, and the type would silently stop being an error.
//
// current is what makes the 409 more than a refusal. A local-first client holds edits
// the server has not seen; to let a person discard theirs, force theirs, or keep
// theirs as a new ticket it needs the other side in the same response, with no second
// request that could itself race a third writer. It is present for an archived ticket
// too, which no read returns - this is the one door to it.
//
// changed_by and changed_at answer "by whom and when" for whichever change caused the
// refusal: the last edit for ticket_conflict, the archive for ticket_archived. They are
// there so a client can say "edited by Sarah 2 minutes ago" without a second lookup.
// changed_by is null when the writer was not recorded or has since been deleted; a
// client that is told the account id equals its own is looking at its own earlier
// write (a retry whose first attempt landed).
type ticketConflict struct {
	huma.ErrorModel
	Code      string     `json:"code" enum:"ticket_conflict,ticket_archived" doc:"ticket_conflict: the ticket was edited since the version you sent. ticket_archived: it was archived." example:"ticket_conflict"`
	Current   ticketBody `json:"current" doc:"The server's copy of the ticket right now, in the shape of GET /tickets/{id} - including when the ticket is archived, which GET never returns."`
	ChangedBy *changedBy `json:"changed_by" doc:"Who made the change that caused this refusal: for ticket_archived, who archived it. Null when not recorded or that account has been deleted."`
	ChangedAt time.Time  `json:"changed_at" doc:"When that change was made."`
}

type changedBy struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
}

// Schema marks the object nullable. huma refuses a nullable tag on a field that is a
// reference to a named struct, and without it the document says changed_by is always
// an object - which a generated client then dereferences, and which is false for a
// ticket whose writer was never recorded.
func (changedBy) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type:                 huma.TypeObject,
		Nullable:             true,
		AdditionalProperties: false,
		Properties: map[string]*huma.Schema{
			"account_id":   {Type: huma.TypeString, Format: "uuid"},
			"display_name": {Type: huma.TypeString},
		},
		Required: []string{"account_id", "display_name"},
	}
}

// ticketError maps the service's errors onto the envelope. Anything else is a fault on
// this side and must not be reported as a 404, or a caller retries forever against a
// database that is down.
func ticketError(ctx context.Context, accounts *account.Service, err error) error {
	if errors.Is(err, ticket.ErrNotFound) {
		return NewError(http.StatusNotFound, CodeTicketNotFound, "no such ticket")
	}

	var conflict *ticket.Conflict
	if errors.As(err, &conflict) {
		return conflictProblem(ctx, accounts, conflict)
	}

	switch {
	case errors.Is(err, ticket.ErrStatusNotFound):
		return newFieldError(CodeStatusNotFound, "the status is not one this ticket's project can use",
			"body.status_id", "no such status for this project")
	case errors.Is(err, ticket.ErrNeighbourNotFound):
		// Only Move raises this, and it knows which of the two members was sent.
		return newFieldError(CodeNeighbourNotFound, "the neighbour is not a ticket in the target column",
			"body.before_id", "no such ticket in the target column")
	case errors.Is(err, ticket.ErrAssigneeNotFound):
		return newFieldError(CodeAssigneeNotFound, "the assignee is not a member of this ticket's workspace",
			"body.assignee_id", "no such assignee in this workspace")
	case errors.Is(err, ticket.ErrEpicNotFound):
		return newFieldError(CodeEpicNotFound, "the epic is not one this ticket's project has",
			"body.epic_id", "no such epic in this project")
	case errors.Is(err, ticket.ErrMilestoneNotFound):
		return newFieldError(CodeMilestoneNotFound, "the milestone is not one this ticket's project has",
			"body.milestone_id", "no such milestone in this project")
	case errors.Is(err, ticket.ErrLabelNotFound):
		loc := "body.label_ids"

		var le *ticket.LabelError
		if errors.As(err, &le) && le.Index >= 0 {
			loc = "body.label_ids[" + strconv.Itoa(le.Index) + "]"
		}

		return newFieldError(CodeLabelNotFound, "a label is not one this ticket's project's team has",
			loc, "no such label for this project's team")
	}

	if e := limitError(err); e != nil {
		return e
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  builds the 409 for a refused conditional write
├─ in ────────────────────────────────────────────
│      ctx         context.Context
│      accounts    *account.Service
│      c           *ticket.Conflict
├─ out ───────────────────────────────────────────
│      error       *ticketConflict · a fault
├─ example ───────────────────────────────────────
│      archived by Sarah  →  409 ticket_archived, changed_by Sarah
*/

// The display name is looked up here, after the transaction, because internal/ticket
// may not read account (see account.Service.DisplayName). The account can vanish in
// between; that reads as not recorded, which is what null already means.
func conflictProblem(ctx context.Context, accounts *account.Service, c *ticket.Conflict) error {
	code, detail := CodeTicketConflict, "the ticket has changed since the version you sent"
	switch {
	case c.Archived && c.Replay:
		code, detail = CodeTicketArchived, "the ticket this Idempotency-Key created has since been archived"
	case c.Archived:
		code, detail = CodeTicketArchived, "the ticket has been archived since the version you sent"
	}

	out := &ticketConflict{
		ErrorModel: huma.ErrorModel{
			Status: http.StatusConflict,
			Title:  http.StatusText(http.StatusConflict),
			Detail: detail,
		},
		Code:      code,
		Current:   asBody(c.Current),
		ChangedAt: c.ChangedAt.UTC(),
	}

	if c.ChangedBy != nil {
		name, err := accounts.DisplayName(ctx, *c.ChangedBy)
		switch {
		case err == nil:
			out.ChangedBy = &changedBy{AccountID: *c.ChangedBy, DisplayName: name}
		case !errors.Is(err, account.ErrNotFound):
			return err
		}
	}

	return out
}

// ifMatchConflict is the 409 an If-Match write can get.
const ifMatchConflict = "Conflict. Only when If-Match was sent: code is ticket_conflict if the ticket was edited since that version, ticket_archived if it was archived. Nothing was written."

// replayConflict is the 409 a create can get.
const replayConflict = "Conflict. Only when Idempotency-Key was sent and had already created a ticket that has since been archived: code is ticket_archived, current is that ticket and changed_by and changed_at say who archived it and when. Nothing was written, and no second ticket exists - restore the original or discard the local copy."

// conflictResponses documents the 409 on an operation that can raise it.
//
// huma documents an error response with the one schema it derives from huma.NewError,
// which is Error and has no room for current, changed_by or changed_at. A field that is
// on the wire and not in the document is invisible to the generated client types, so the
// operations that can raise a conflict declare their own 409 with ticketConflict's
// schema. That also means supplying the default response, since huma only adds it for
// an operation that declares none.
func conflictResponses(api huma.API, description string) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	problem := func(schema *huma.Schema) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{"application/problem+json": {Schema: schema}}
	}

	return map[string]*huma.Response{
		"409": {
			Description: description,
			Content:     problem(registry.Schema(reflect.TypeOf(ticketConflict{}), true, "TicketConflict")),
		},
		"default": {
			Description: "Error",
			Content:     problem(registry.Schema(reflect.TypeOf(Error{}), true, "Error")),
		},
	}
}

/*
┌─ api ───────────────────────────────────────────
│  registers the five ticket operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *ticket.Service    nil only for cmd/openapi
│      accounts  *account.Service  names the writer in a 409
│      feed      ticketFeed        publishes each committed write
├─ example ───────────────────────────────────────
│      →  POST /api/v1/tickets, GET /api/v1/tickets/{id}
*/

func registerTickets(api huma.API, svc *ticket.Service, accounts *account.Service, feed ticketFeed) {
	// Every operation requires a caller. There is no public ticket endpoint, and
	// marking them individually rather than wrapping the group means a new one is
	// unprotected only if someone omits the line - which the middleware test catches.
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets",
		Summary:     "Create a ticket (safe to retry with Idempotency-Key)",
		Description: "Send `Idempotency-Key` to make the create safe to retry after a lost response. " +
			"The first request with a key creates the ticket and answers `Idempotent-Replayed: false`; " +
			"any later request with the same key from the same account creates nothing and answers " +
			"`Idempotent-Replayed: true` with the original ticket as it is now - same status, 200, as a " +
			"fresh create. A replay publishes nothing on the change feed. If the original has been archived " +
			"the answer is a 409 `ticket_archived` carrying it. The body of a replay is ignored.",
		Metadata:  requires,
		Responses: conflictResponses(api, replayConflict),
	}, func(ctx context.Context, in *createTicketInput) (*createTicketOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, replayed, err := svc.CreateIdempotent(ctx, me.AccountID, ticket.CreateParams{
			ProjectID:      in.Body.ProjectID,
			Title:          in.Body.Title,
			Description:    in.Body.Description,
			StatusID:       in.Body.StatusID,
			AssigneeID:     in.Body.AssigneeID,
			EpicID:         in.Body.EpicID,
			MilestoneID:    in.Body.MilestoneID,
			LabelIDs:       in.Body.LabelIDs,
			IdempotencyKey: in.key(),
		})
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		// A replay wrote nothing, so there is nothing to announce: the original create
		// already told the workspace, and a second ticket.created for the same ticket
		// would make every watcher apply an insert twice.
		if !replayed {
			feed.publish(ctx, realtime.TicketCreated, me.AccountID, t)
		}

		out := &createTicketOutput{ETag: etag(t.Version), Body: asBody(t)}
		if in.key() != nil {
			out.Replayed = strconv.FormatBool(replayed)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-ticket",
		Method:      http.MethodGet,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Fetch one ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *getTicketInput) (*ticketDetailOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		docs, err := svc.LinkedDocuments(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		return &ticketDetailOutput{ETag: etag(t.Version), Body: ticketDetailBody{TicketBody: asBody(t), Documents: asLinkedDocuments(docs)}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "link-ticket-document",
		Method:        http.MethodPost,
		Path:          V1 + "/tickets/{id}/documents",
		Summary:       "Link a document to a ticket",
		Description:   "Idempotent: linking a pair that is already linked is a 204 and changes nothing. An archived ticket or document links like a live one, and archiving either later does not remove the link. Both must be in a workspace the caller belongs to, and the same one; otherwise `not_found`, which does not say which was missing. Publishes `ticket.document_linked` when a link was made.",
		DefaultStatus: http.StatusNoContent,
		Metadata:      requires,
	}, func(ctx context.Context, in *linkDocumentInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.LinkDocument(ctx, me.AccountID, in.ID, in.Body.DocumentID)
		if err != nil {
			return nil, linkNotFound(err)
		}

		if res.Changed {
			feed.publishLink(ctx, realtime.TicketDocumentLinked, me.AccountID, res.ProjectID, in.ID, in.Body.DocumentID)
		}

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "unlink-ticket-document",
		Method:        http.MethodDelete,
		Path:          V1 + "/tickets/{id}/documents/{document_id}",
		Summary:       "Unlink a document from a ticket",
		Description:   "Idempotent: unlinking a pair that is not linked is a 204. Only an unknown ticket, or one outside the caller's workspaces, is `not_found`. Publishes `ticket.document_unlinked` when a link was removed.",
		DefaultStatus: http.StatusNoContent,
		Metadata:      requires,
	}, func(ctx context.Context, in *unlinkDocumentInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		res, err := svc.UnlinkDocument(ctx, me.AccountID, in.ID, in.DocumentID)
		if err != nil {
			return nil, linkNotFound(err)
		}

		if res.Changed {
			feed.publishLink(ctx, realtime.TicketDocumentUnlinked, me.AccountID, res.ProjectID, in.ID, in.DocumentID)
		}

		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-ticket",
		Method:      http.MethodPut,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Replace a ticket's writable fields",
		Description: "Last write wins unless `If-Match` is sent. With `If-Match: \"<version>\"` the write happens only if the ticket is still at that version; otherwise it is refused with a 409 (`ticket_conflict` or `ticket_archived`) that carries the current ticket, who changed it and when. Without it, a write to an archived ticket is `ticket_not_found`, as it always was.",
		Metadata:    requires,
		Responses:   conflictResponses(api, ifMatchConflict),
	}, func(ctx context.Context, in *updateTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		// PUT, not PATCH: UpdateParams replaces wholesale, and the method should say
		// so. A caller omitting status_id clears it, which is what PUT means.
		t, err := svc.Update(ctx, me.AccountID, ticket.UpdateParams{
			ID:          in.ID,
			Title:       in.Body.Title,
			Description: in.Body.Description,
			StatusID:    in.Body.StatusID,
			AssigneeID:  in.Body.AssigneeID,
			Epic:        ticket.Ref{Set: in.Body.EpicID.Set, ID: in.Body.EpicID.Value},
			Milestone:   ticket.Ref{Set: in.Body.MilestoneID.Set, ID: in.Body.MilestoneID.Value},
			LabelIDs:    in.Body.LabelIDs,
			IfVersion:   ifVersion,
		})
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)

		return asOutput(t), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "move-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets/{id}/move",
		Summary:     "Drop a ticket into a board column, at a place in it",
		Description: "One write per drop: sets `status_id` and `position` together. Send at most one of `position`, `before_id` and `after_id` " +
			"(more is a 422); none puts the ticket at the end of the column. `before_id`/`after_id` name a live ticket in the target column, " +
			"or the answer is a 422 `neighbour_not_found` at that member. Written like any edit: version + 1, announced as `ticket.updated`, and " +
			"`If-Match` makes it conditional. When there is no room between two neighbours the target column's other tickets are renumbered; " +
			"each of those is written like an edit (version + 1) and announced as `ticket.updated`, before the moved ticket's own event.",
		Metadata:  requires,
		Responses: conflictResponses(api, ifMatchConflict),
	}, func(ctx context.Context, in *moveTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		sent := 0
		for _, set := range []bool{in.Body.Position != nil, in.Body.BeforeID != nil, in.Body.AfterID != nil} {
			if set {
				sent++
			}
		}
		if sent > 1 {
			return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"send at most one of position, before_id and after_id",
				"body", "position, before_id and after_id are mutually exclusive")
		}

		t, renumbered, err := svc.Move(ctx, me.AccountID, ticket.MoveParams{
			ID:        in.ID,
			StatusID:  in.Body.StatusID,
			Position:  in.Body.Position,
			BeforeID:  in.Body.BeforeID,
			AfterID:   in.Body.AfterID,
			IfVersion: ifVersion,
		})
		if errors.Is(err, ticket.ErrNeighbourNotFound) && in.Body.AfterID != nil {
			return nil, newFieldError(CodeNeighbourNotFound, "the neighbour is not a ticket in the target column",
				"body.after_id", "no such ticket in the target column")
		}
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		for _, o := range renumbered {
			feed.publish(ctx, realtime.TicketUpdated, me.AccountID, o)
		}
		feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)

		return asOutput(t), nil
	})

	// DELETE archives. The verb is what a client expects for "remove this", and 0029
	// is why the row survives - a hard delete CASCADEs to comment and takes the
	// discussion with it. The summary says so, because a DELETE that does not delete
	// is exactly the sort of thing a reader should not have to discover.
	huma.Register(api, huma.Operation{
		OperationID: "archive-ticket",
		Method:      http.MethodDelete,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Archive a ticket (soft delete - the row and its comments survive)",
		Description: "Takes `If-Match` like the update does. A ticket that is already archived answers `ticket_archived` when `If-Match` is sent - including when the archive was your own and the response was lost, in which case `changed_by` is you - and `ticket_not_found` when it is not. The ticket's new version is in the `ETag` header.",
		Metadata:    requires,
		Responses:   conflictResponses(api, ifMatchConflict),
	}, func(ctx context.Context, in *writeTicketInput) (*versionOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		t, err := svc.Archive(ctx, me.AccountID, in.ID, ifVersion)
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		feed.publish(ctx, realtime.TicketArchived, me.AccountID, t)

		return &versionOutput{ETag: etag(t.Version)}, nil
	})

	// The list operation is registered through the generic path rather than spelled
	// out here: the body, the envelope and the error mapping are the same for every
	// resource, and only the vocabulary and the row mapping are ticket's.
	registerList(api, listSpec[ticketBody]{
		OperationID: "list-tickets",
		Path:        V1 + "/tickets/list",
		Summary:     "List tickets (a read - see the description for why it is a POST)",
		Fields:      ticket.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[ticketBody], error) {
			p := ticket.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.List(ctx, accountID, p)
			if err != nil {
				return listPage[ticketBody]{}, err
			}

			items := make([]ticketBody, len(page.Tickets))
			for i, t := range page.Tickets {
				items[i] = asBody(t)
			}

			return listPage[ticketBody]{
				Items:      items,
				NextCursor: page.NextCursor,
				HasMore:    page.HasMore,
			}, nil
		},
	})

	huma.Register(api, huma.Operation{
		OperationID: "unarchive-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets/{id}/unarchive",
		Summary:     "Restore an archived ticket",
		Description: "Takes `If-Match`: the version of the archived ticket you last saw. If it was restored, edited or archived again since, the answer is a 409 `ticket_conflict` with the current ticket. Restoring a ticket that is not archived is `ticket_not_found` either way. The ticket's new version is in the `ETag` header.",
		Metadata:    requires,
		Responses:   conflictResponses(api, ifMatchConflict),
	}, func(ctx context.Context, in *writeTicketInput) (*versionOutput, error) {
		me, _ := auth.FromContext(ctx)

		ifVersion, err := in.ifVersion()
		if err != nil {
			return nil, err
		}

		t, err := svc.Unarchive(ctx, me.AccountID, in.ID, ifVersion)
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		feed.publish(ctx, realtime.TicketUnarchived, me.AccountID, t)

		return &versionOutput{ETag: etag(t.Version)}, nil
	})
}

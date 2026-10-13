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

	UpdatedAt time.Time `json:"updated_at" doc:"When the ticket last changed - an edit, an archive or a restore. For display; to compare versions use version."`
	UpdatedBy *string   `json:"updated_by" doc:"The account that made the last change. Null when it was not recorded (tickets from before this field existed) or that account has since been deleted - show it as someone."`
	Version   int       `json:"version" minimum:"1" doc:"Starts at 1 and rises by exactly 1 on every update, archive and restore. Send it back as If-Match to make a write conditional; also in the ETag header. See the If-Match parameter."`
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

type createTicketInput struct {
	Body struct {
		ProjectID   string  `json:"project_id" required:"true" format:"uuid"`
		Title       string  `json:"title" required:"true"`
		Description string  `json:"description"`
		StatusID    *string `json:"status_id" format:"uuid"`
		AssigneeID  *string `json:"assignee_id" format:"uuid"`
	}
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
		Title       string  `json:"title" required:"true"`
		Description string  `json:"description"`
		StatusID    *string `json:"status_id" format:"uuid"`
		AssigneeID  *string `json:"assignee_id" format:"uuid"`
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
		UpdatedAt:   t.UpdatedAt.UTC(),
		UpdatedBy:   t.UpdatedBy,
		Version:     t.Version,
	}
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
	if c.Archived {
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

// conflictResponses documents the 409 on an operation that can raise it.
//
// huma documents an error response with the one schema it derives from huma.NewError,
// which is Error and has no room for current, changed_by or changed_at. A field that is
// on the wire and not in the document is invisible to the generated client types, so the
// operations that can raise a conflict declare their own 409 with ticketConflict's
// schema. That also means supplying the default response, since huma only adds it for
// an operation that declares none.
func conflictResponses(api huma.API) map[string]*huma.Response {
	registry := api.OpenAPI().Components.Schemas
	problem := func(schema *huma.Schema) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{"application/problem+json": {Schema: schema}}
	}

	return map[string]*huma.Response{
		"409": {
			Description: "Conflict. Only when If-Match was sent: code is ticket_conflict if the ticket was edited since that version, ticket_archived if it was archived. Nothing was written.",
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
├─ example ───────────────────────────────────────
│      →  POST /api/v1/tickets, GET /api/v1/tickets/{id}
*/

func registerTickets(api huma.API, svc *ticket.Service, accounts *account.Service) {
	// Every operation requires a caller. There is no public ticket endpoint, and
	// marking them individually rather than wrapping the group means a new one is
	// unprotected only if someone omits the line - which the middleware test catches.
	requires := map[string]any{RequireAuth: true}

	huma.Register(api, huma.Operation{
		OperationID: "create-ticket",
		Method:      http.MethodPost,
		Path:        V1 + "/tickets",
		Summary:     "Create a ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *createTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Create(ctx, me.AccountID, ticket.CreateParams{
			ProjectID:   in.Body.ProjectID,
			Title:       in.Body.Title,
			Description: in.Body.Description,
			StatusID:    in.Body.StatusID,
			AssigneeID:  in.Body.AssigneeID,
		})
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		return asOutput(t), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-ticket",
		Method:      http.MethodGet,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Fetch one ticket",
		Metadata:    requires,
	}, func(ctx context.Context, in *getTicketInput) (*ticketOutput, error) {
		me, _ := auth.FromContext(ctx)

		t, err := svc.Get(ctx, me.AccountID, in.ID)
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

		return asOutput(t), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-ticket",
		Method:      http.MethodPut,
		Path:        V1 + "/tickets/{id}",
		Summary:     "Replace a ticket's writable fields",
		Description: "Last write wins unless `If-Match` is sent. With `If-Match: \"<version>\"` the write happens only if the ticket is still at that version; otherwise it is refused with a 409 (`ticket_conflict` or `ticket_archived`) that carries the current ticket, who changed it and when. Without it, a write to an archived ticket is `ticket_not_found`, as it always was.",
		Metadata:    requires,
		Responses:   conflictResponses(api),
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
			IfVersion:   ifVersion,
		})
		if err != nil {
			return nil, ticketError(ctx, accounts, err)
		}

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
		Responses:   conflictResponses(api),
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
		Responses:   conflictResponses(api),
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

		return &versionOutput{ETag: etag(t.Version)}, nil
	})
}

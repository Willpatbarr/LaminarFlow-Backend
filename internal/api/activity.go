/*
╔═ activity.go ═════════════════════════════════════════════════════════════════════════
║  http handlers · the activity log
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      activityBody       struct
║      asActivityBody     func
║      registerActivity   func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  POST /api/v1/activity/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/activity"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
)

// activityBody is one entry of a ticket's or a document's history (LAM-89).
type activityBody struct {
	ID            string    `json:"id"`
	EntityType    string    `json:"entity_type" enum:"ticket,document"`
	EntityID      string    `json:"entity_id" format:"uuid"`
	ActorID       *string   `json:"actor_id" nullable:"true" doc:"The account that made the change; a token acts as its owner. Null when the account has been deleted or the write had none: render \"someone\"."`
	Kind          string    `json:"kind" enum:"created,status_changed,assignee_changed,parent_changed,label_added,label_removed,archived,unarchived,aspect_type_changed,field_changed" doc:"What happened. Document entries are only created, archived, unarchived, aspect_type_changed and field_changed."`
	Field         *string   `json:"field" nullable:"true" doc:"Only for field_changed: ticket - title, description or milestone; document - title or fields."`
	SummaryBefore *string   `json:"summary_before" nullable:"true" doc:"The value before the change; null for none. status_changed, assignee_changed, parent_changed (the epic), label_removed, aspect_type_changed and milestone carry the id of the thing, so they stay true after a rename. title and description carry the text, cut to 200 characters. A document's fields carry the sorted ids of the fields that had a value."`
	SummaryAfter  *string   `json:"summary_after" nullable:"true" doc:"The value after the change, in the same form. label_added carries the label id here."`
	CreatedAt     time.Time `json:"created_at"`
}

func asActivityBody(e activity.Entry) activityBody {
	return activityBody{
		ID:            e.ID,
		EntityType:    e.EntityType(),
		EntityID:      e.EntityID(),
		ActorID:       e.ActorID,
		Kind:          e.Kind,
		Field:         e.Field,
		SummaryBefore: e.Before,
		SummaryAfter:  e.After,
		CreatedAt:     e.CreatedAt.UTC(),
	}
}

// publishActivity announces log entries a committed write made, one activity.created each.
// The caller passes the entity's workspace, which it already has. After the commit and
// best-effort, like every other publish; a client that misses one lists again.
func (f ticketFeed) publishActivity(workspaceID, actorID string, entries []activity.Entry) {
	if f.hub == nil {
		return
	}

	for _, e := range entries {
		ev := realtime.Event{
			Type:       realtime.ActivityCreated,
			ActorID:    actorID,
			At:         e.CreatedAt.UTC(),
			ActivityID: e.ID,
			Activity:   asActivityBody(e),
		}
		if e.TicketID != nil {
			ev.TicketID = *e.TicketID
		} else {
			ev.DocumentID = *e.DocumentID
		}

		f.hub.Publish(workspaceID, ev)
	}
}

/*
┌─ api ───────────────────────────────────────────
│  registers the activity operations
├─ in ────────────────────────────────────────────
│      api        huma.API
│      svc        *activity.Service     nil only for cmd/openapi
│      tickets    *ticket.Service       proves the caller may see the ticket
│      docs       *document.Service     proves the caller may see the document
├─ example ───────────────────────────────────────
│      →  POST /api/v1/activity/list
*/

// Tokens: ticket:read. A history is a read of a ticket or a document, and documents already
// sit under the ticket scopes (get-document), so no new scope is minted: a script that may
// read a ticket may read what happened to it. There is no write operation - entries are made
// only by the changes they describe.
func registerActivity(api huma.API, svc *activity.Service, tickets *ticket.Service, docs *document.Service) {
	registerList(api, listSpec[activityBody]{
		OperationID: "list-activity",
		Permission:  auth.TicketRead,
		Path:        V1 + "/activity/list",
		Summary:     "List the history of one ticket or document (a read - see the description for why it is a POST)",
		Fields:      activity.Fields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[activityBody], error) {
			p := activity.ListParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			// The entity first: a request that names none is a 422, and one that names an
			// entity the caller cannot see is a 404 that does not say whether it exists.
			entityType, entityID, err := activity.Scope(p.Filter)
			if err != nil {
				return listPage[activityBody]{}, err
			}

			switch entityType {
			case activity.EntityTicket:
				_, err = tickets.Get(ctx, accountID, entityID)
			default:
				_, err = docs.Get(ctx, accountID, entityID)
			}
			if errors.Is(err, ticket.ErrNotFound) || errors.Is(err, document.ErrNotFound) {
				return listPage[activityBody]{}, NewError(http.StatusNotFound, defaultCode(http.StatusNotFound),
					"no such "+entityType)
			}
			if err != nil {
				return listPage[activityBody]{}, err
			}

			page, err := svc.List(ctx, p)
			if err != nil {
				return listPage[activityBody]{}, err
			}

			items := make([]activityBody, len(page.Entries))
			for i, e := range page.Entries {
				items[i] = asActivityBody(e)
			}

			return listPage[activityBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})
}

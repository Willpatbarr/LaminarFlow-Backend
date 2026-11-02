/*
╔═ mcp_tools.go ════════════════════════════════════════════════════════════════════════
║  the MCP tools (LAM-197): arguments in, a service call, the REST body out
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      newMCPServer        func
║      <tool>Args          one struct per tool
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      mcpHandler  →  newMCPServer
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/comment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A curated set, not one tool per endpoint: an agent that is offered eighty tools chooses
// badly. Each tool needs the scope its REST endpoint needs (a list of sprints is ticket:read
// because POST /sprints/list is), and every tool is a few lines of argument handling around
// the service call the handler makes. Write tools announce what they wrote with the same
// ticketFeed calls as the handlers, after the service has committed, and skip the
// announcement for an idempotent replay.
//
// Ids are UUIDs. Bodies - descriptions, comments, documents - are Markdown (ADR 0006).

const mcpInstructions = "LaminarFlow tickets, comments and documents. Every id is a UUID; get them from " +
	"list_teams, list_projects, list_statuses, list_sprints, list_labels, list_members, list_tickets or search. " +
	"Descriptions, comments and document bodies are Markdown. A tool that fails returns isError with a JSON " +
	"object whose `code` is stable (for example insufficient_scope, ticket_not_found, ticket_conflict)."

// pageArgs is the paging every list tool shares.
type pageArgs struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"rows per page, 1 to 200; default 50"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page; pass it back unchanged"`
}

// page is a list tool's answer.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor" jsonschema:"null on the last page"`
	HasMore    bool    `json:"has_more"`
}

func pageOf[T any](items []T, next string, more bool) page[T] {
	if items == nil {
		items = []T{}
	}

	p := page[T]{Items: items, HasMore: more}
	if next != "" {
		p.NextCursor = &next
	}

	return p
}

// resolveMe resolves the alias "me" to the caller, so an agent can say "my tickets" without first
// looking up its own account id.
func resolveMe(me auth.Identity, v string) string {
	if v == "me" {
		return me.AccountID
	}

	return v
}

type searchArgs struct {
	Q     string `json:"q" jsonschema:"what to look for; words are stemmed and the last word also matches as a prefix; quoted phrases, or, and -minus work"`
	Type  string `json:"type,omitempty" jsonschema:"only this kind of result: ticket, document, comment or project"`
	Limit int    `json:"limit,omitempty" jsonschema:"1 to 50; default 20"`
}

type listTicketsArgs struct {
	pageArgs
	TeamID     string `json:"team_id,omitempty" jsonschema:"only tickets of this team's projects"`
	ProjectID  string `json:"project_id,omitempty"`
	StatusID   string `json:"status_id,omitempty" jsonschema:"a status id from list_statuses"`
	AssigneeID string `json:"assignee_id,omitempty" jsonschema:"an account id from list_members, or the word me"`
	LabelID    string `json:"label_id,omitempty" jsonschema:"a label id from list_labels"`
	SprintID   string `json:"sprint_id,omitempty" jsonschema:"a sprint id from list_sprints"`
	Text       string `json:"text,omitempty" jsonschema:"tickets whose title or description contains this text"`
}

type idArgs struct {
	ID string `json:"id" jsonschema:"the id (a UUID)"`
}

type createTicketArgs struct {
	ProjectID      string   `json:"project_id" jsonschema:"the project to file the ticket in, from list_projects"`
	Title          string   `json:"title" jsonschema:"1 to 200 characters"`
	Description    string   `json:"description,omitempty" jsonschema:"Markdown, up to 50000 characters"`
	StatusID       string   `json:"status_id,omitempty" jsonschema:"a status of the project's team, from list_statuses"`
	AssigneeID     string   `json:"assignee_id,omitempty" jsonschema:"an account id from list_members, or the word me"`
	EpicID         string   `json:"epic_id,omitempty" jsonschema:"the parent epic, an epic of the same project"`
	LabelIDs       []string `json:"label_ids,omitempty" jsonschema:"labels of the project's team, from list_labels"`
	IdempotencyKey string   `json:"idempotency_key,omitempty" jsonschema:"makes a retry safe: the same key creates the ticket once"`
}

type updateTicketArgs struct {
	ID          string    `json:"id" jsonschema:"the ticket id"`
	Title       *string   `json:"title,omitempty" jsonschema:"new title; omit to keep"`
	Description *string   `json:"description,omitempty" jsonschema:"new Markdown description, replacing the old one; omit to keep"`
	AssigneeID  *string   `json:"assignee_id,omitempty" jsonschema:"an account id, me, or none to unassign; omit to keep"`
	EpicID      *string   `json:"epic_id,omitempty" jsonschema:"the parent epic's id, or none to clear; omit to keep"`
	LabelIDs    *[]string `json:"label_ids,omitempty" jsonschema:"the complete set of labels; an empty list clears them; omit to keep"`
}

type moveTicketArgs struct {
	ID       string `json:"id" jsonschema:"the ticket id"`
	StatusID string `json:"status_id,omitempty" jsonschema:"the status to move to, from list_statuses"`
	Status   string `json:"status,omitempty" jsonschema:"or the status by name, for example In Progress; matched within the ticket's team, ignoring case"`
	BeforeID string `json:"before_id,omitempty" jsonschema:"optional: land just above this ticket in the target column"`
	AfterID  string `json:"after_id,omitempty" jsonschema:"optional: land just below this ticket in the target column"`
}

type listCommentsArgs struct {
	pageArgs
	TicketID   string `json:"ticket_id,omitempty" jsonschema:"the ticket whose comments to list"`
	DocumentID string `json:"document_id,omitempty" jsonschema:"or the document whose comments to list"`
}

type addCommentArgs struct {
	TicketID       string `json:"ticket_id,omitempty" jsonschema:"the ticket to comment on; send exactly one of ticket_id and document_id"`
	DocumentID     string `json:"document_id,omitempty" jsonschema:"the document to comment on"`
	Body           string `json:"body" jsonschema:"the comment, Markdown, up to 50000 characters"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"makes a retry safe: the same key comments once"`
}

type saveDocumentArgs struct {
	ID             string            `json:"id,omitempty" jsonschema:"the document to edit; omit to create a new one"`
	Title          *string           `json:"title,omitempty" jsonschema:"up to 200 characters; omit to keep (an update) or leave untitled (a create)"`
	Content        *string           `json:"content,omitempty" jsonschema:"the Markdown text of a normal document, replacing it; omit to keep"`
	Fields         map[string]string `json:"fields,omitempty" jsonschema:"for an aspect document: field id to Markdown value; fields not named are kept"`
	ProjectID      string            `json:"project_id,omitempty" jsonschema:"create only: file the new document under this project"`
	TeamID         string            `json:"team_id,omitempty" jsonschema:"create only: file the new document under this team"`
	IfVersion      int               `json:"if_version,omitempty" jsonschema:"update only: refuse with document_conflict unless the document is still at this version; defaults to the version just read"`
	IdempotencyKey string            `json:"idempotency_key,omitempty" jsonschema:"create only: makes a retry safe"`
}

type teamIDArgs struct {
	TeamID string `json:"team_id" jsonschema:"the team, from list_teams"`
}

type createTeamArgs struct {
	Name string `json:"name" jsonschema:"1 to 100 characters; unique within the workspace"`
}

type createProjectArgs struct {
	TeamID string `json:"team_id" jsonschema:"the team the project belongs to, from list_teams"`
	Name   string `json:"name" jsonschema:"1 to 100 characters"`
}

type projectIDArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project, from list_projects"`
}

/*
┌─ api ───────────────────────────────────────────
│  builds the MCP server with every tool registered
├─ in ────────────────────────────────────────────
│      d    *mcpDeps    may be nil: tools are built, none run
├─ out ───────────────────────────────────────────
│      *mcp.Server
│      map[string]auth.Permission    each tool's scope, for tools/list
*/

func newMCPServer(d *mcpDeps) (*mcp.Server, map[string]auth.Permission) {
	s := mcp.NewServer(&mcp.Implementation{Name: "laminarflow", Version: SpecVersion},
		&mcp.ServerOptions{Instructions: mcpInstructions, SchemaCache: mcp.NewSchemaCache()})
	perms := map[string]auth.Permission{}

	if d == nil {
		d = &mcpDeps{}
	}

	registerMCPReads(s, perms, d)
	registerMCPWrites(s, perms, d)

	return s, perms
}

func registerMCPReads(s *mcp.Server, perms map[string]auth.Permission, d *mcpDeps) {
	addMCPTool(s, perms, "search",
		"Full-text search across tickets, documents, comments and projects, best match first. Returns up to `limit` hits "+
			"{type, id, title, snippet, parent_type, parent_id}; for a comment hit parent_id is the ticket or document it is on. "+
			"Takes free text, not ids. Follow up with get_ticket or get_document.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in searchArgs) (any, error) {
			hits, err := d.search.Find(ctx, me.AccountID, search.FindParams{Q: in.Q, Type: in.Type, Limit: in.Limit})
			if err != nil {
				if e := limitError(err); e != nil {
					return nil, e
				}

				return nil, err
			}

			items := make([]searchItem, len(hits))
			for i, h := range hits {
				items[i] = searchItem{Type: h.Type, ID: h.ID, Title: h.Title, Snippet: h.Snippet,
					FieldLabel: h.FieldLabel, ParentType: h.ParentType, ParentID: h.ParentID, Score: h.Score}
			}

			return map[string]any{"items": items}, nil
		})

	addMCPTool(s, perms, "list_tickets",
		"Lists tickets, most recently updated first, one page at a time. Every filter is optional and they combine with AND. "+
			"Returns {items, next_cursor, has_more}; each item is {id, project_id, title, description (Markdown), status_id, assignee_id, "+
			"epic_id, milestone_id, label_ids, version, updated_at}. Pass next_cursor back as cursor for the next page. "+
			"Filter ids come from list_teams, list_projects, list_statuses, list_labels, list_sprints and list_members.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in listTicketsArgs) (any, error) {
			g := filter.Group{Op: filter.And}
			eq := func(field, v string) {
				g.Conditions = append(g.Conditions, filter.Condition{Field: field, Op: filter.Eq, Value: v})
			}

			if in.TeamID != "" {
				// A ticket belongs to a project, and a project to a team, so a team is a set of
				// projects. No tickets filter on team directly.
				projects, err := d.projects.List(ctx, me.AccountID, project.ListParams{
					Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{{Field: "team", Op: filter.Eq, Value: in.TeamID}}},
					Limit:  200,
				})
				if err != nil {
					return nil, listError(err)
				}
				if len(projects.Projects) == 0 {
					return pageOf[ticketBody](nil, "", false), nil
				}

				ids := make([]any, len(projects.Projects))
				for i, p := range projects.Projects {
					ids[i] = p.ID
				}
				g.Conditions = append(g.Conditions, filter.Condition{Field: "project", Op: filter.In, Value: ids})
			}

			for field, v := range map[string]string{
				"project": in.ProjectID, "status": in.StatusID, "assignee": resolveMe(me, in.AssigneeID),
				"label": in.LabelID, "sprint": in.SprintID,
			} {
				if v != "" {
					eq(field, v)
				}
			}

			if in.Text != "" {
				g.Groups = append(g.Groups, filter.Group{Op: filter.Or, Conditions: []filter.Condition{
					{Field: "title", Op: filter.Contains, Value: in.Text},
					{Field: "description", Op: filter.Contains, Value: in.Text},
				}})
			}

			pg, err := d.tickets.List(ctx, me.AccountID, ticket.ListParams{Filter: g, Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, listError(err)
			}

			items := make([]ticketBody, len(pg.Tickets))
			for i, t := range pg.Tickets {
				items[i] = asBody(t)
			}

			return pageOf(items, pg.NextCursor, pg.HasMore), nil
		})

	addMCPTool(s, perms, "get_ticket",
		"Fetches one ticket by id: {id, project_id, title, description (Markdown), status_id, assignee_id, epic_id, milestone_id, "+
			"label_ids, version, updated_at, documents}. `documents` lists the documents linked to it as {id, title, type, archived_at}; "+
			"read one with get_document. Archived tickets are not found.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in idArgs) (any, error) {
			t, err := d.tickets.Get(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			docs, err := d.tickets.LinkedDocuments(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			return ticketDetailBody{TicketBody: asBody(t), Documents: asLinkedDocuments(docs)}, nil
		})

	addMCPTool(s, perms, "list_comments",
		"Lists the comments on a ticket or a document, oldest first. Pass ticket_id or document_id. Returns {items, next_cursor, has_more}; "+
			"each item is {id, author_id, body (Markdown), created_at, edited_at}.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in listCommentsArgs) (any, error) {
			if (in.TicketID == "") == (in.DocumentID == "") {
				return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
					"send exactly one of ticket_id and document_id", "arguments", "exactly one of ticket_id and document_id is required")
			}

			cond := filter.Condition{Field: "ticket_id", Op: filter.Eq, Value: in.TicketID}
			if in.DocumentID != "" {
				cond = filter.Condition{Field: "document_id", Op: filter.Eq, Value: in.DocumentID}
			}

			pg, err := d.comments.List(ctx, me.AccountID, comment.ListParams{
				Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{cond}}, Limit: in.Limit, Cursor: in.Cursor,
			})
			if err != nil {
				return nil, listError(err)
			}

			items := make([]commentBody, len(pg.Comments))
			for i, c := range pg.Comments {
				items[i] = asCommentBody(c)
			}

			return pageOf(items, pg.NextCursor, pg.HasMore), nil
		})

	addMCPTool(s, perms, "get_document",
		"Fetches one document by id: {id, title, type, team_id, project_id, version, body, tickets}. `body` maps a field id to a Markdown string; "+
			"a normal document keeps its text under the key content. `tickets` lists the tickets linked to it. Edit it with save_document.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in idArgs) (any, error) {
			doc, err := d.documents.Get(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, documentError(err)
			}

			linked, err := d.tickets.LinkedTickets(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, err
			}

			tickets := make([]linkedTicketBody, len(linked))
			for i, t := range linked {
				tickets[i] = linkedTicketBody{ID: t.ID, Title: t.Title, ArchivedAt: utcOrNil(t.ArchivedAt)}
			}

			return documentDetailBody{DocumentBody: asDocumentBody(doc), Tickets: tickets}, nil
		})

	addMCPTool(s, perms, "list_teams",
		"Lists the workspace's teams with their statuses: {id, name, statuses: [{id, name, category, position}]}. Start here to find "+
			"team and status ids.",
		auth.TeamRead, false, func(ctx context.Context, me auth.Identity, in pageArgs) (any, error) {
			pg, err := d.teams.List(ctx, me.AccountID, team.ListParams{Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, listError(err)
			}

			items := make([]teamBody, len(pg.Teams))
			for i, t := range pg.Teams {
				items[i] = asTeamBody(t)
			}

			return pageOf(items, pg.NextCursor, pg.HasMore), nil
		})

	addMCPTool(s, perms, "list_statuses",
		"Lists one team's statuses in board order: {id, name, category (not_started, in_progress or done), position}. "+
			"A ticket's status_id is one of these; move_ticket also takes the name.",
		auth.TeamRead, false, func(ctx context.Context, me auth.Identity, in teamIDArgs) (any, error) {
			t, err := d.teams.Get(ctx, me.AccountID, in.TeamID)
			if err != nil {
				return nil, teamError(err)
			}

			return asTeamBody(t).Statuses, nil
		})

	addMCPTool(s, perms, "list_projects",
		"Lists the workspace's projects by name: {id, team_id, name}. A ticket is filed in a project, and a project belongs to a team.",
		auth.ProjectRead, false, func(ctx context.Context, me auth.Identity, in pageArgs) (any, error) {
			pg, err := d.projects.List(ctx, me.AccountID, project.ListParams{Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, listError(err)
			}

			items := make([]projectBody, len(pg.Projects))
			for i, p := range pg.Projects {
				items[i] = asProjectBody(p)
			}

			return pageOf(items, pg.NextCursor, pg.HasMore), nil
		})

	addMCPTool(s, perms, "list_sprints",
		"Lists a project's sprints, soonest first: {id, project_id, name, start_date, end_date}. Use a sprint id as list_tickets' sprint_id.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in projectIDArgs) (any, error) {
			sprints, err := d.sprints.List(ctx, me.AccountID, in.ProjectID)
			if err != nil {
				return nil, sprintError(err)
			}

			items := make([]sprintBody, len(sprints))
			for i, sp := range sprints {
				items[i] = asSprintBody(sp)
			}

			return pageOf(items, "", false), nil
		})

	addMCPTool(s, perms, "list_labels",
		"Lists a team's labels by name: {id, team_id, name}. Use label ids in create_ticket, update_ticket and list_tickets.",
		auth.TicketRead, false, func(ctx context.Context, me auth.Identity, in teamIDArgs) (any, error) {
			labels, err := d.labels.List(ctx, me.AccountID, in.TeamID)
			if err != nil {
				return nil, labelError(err)
			}

			items := make([]labelBody, len(labels))
			for i, l := range labels {
				items[i] = asLabelBody(l)
			}

			return pageOf(items, "", false), nil
		})

	addMCPTool(s, perms, "list_members",
		"Lists the people in the workspace: {account_id, display_name, email, role}. A ticket's assignee_id and a comment's author_id are "+
			"account ids from here.",
		auth.TeamRead, false, func(ctx context.Context, me auth.Identity, in pageArgs) (any, error) {
			pg, err := d.accounts.Members(ctx, me.AccountID, account.MemberParams{Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, listError(err)
			}

			items := make([]memberBody, len(pg.Members))
			for i, m := range pg.Members {
				items[i] = memberBody{AccountID: m.AccountID, DisplayName: m.DisplayName, Email: m.Email, Role: m.Role, CreatedAt: m.JoinedAt}
			}

			return pageOf(items, pg.NextCursor, pg.HasMore), nil
		})
}

func registerMCPWrites(s *mcp.Server, perms map[string]auth.Permission, d *mcpDeps) {
	addMCPTool(s, perms, "create_ticket",
		"Creates a ticket in a project and returns it (see get_ticket for the shape). Only project_id and title are required. "+
			"Send idempotency_key to make a retry safe. The new ticket appears in the activity list and live in open browsers.",
		auth.TicketWrite, true, func(ctx context.Context, me auth.Identity, in createTicketArgs) (any, error) {
			p := ticket.CreateParams{ProjectID: in.ProjectID, Title: in.Title, Description: in.Description,
				StatusID: optional(in.StatusID), AssigneeID: optional(resolveMe(me, in.AssigneeID)),
				EpicID: optional(in.EpicID), LabelIDs: in.LabelIDs, IdempotencyKey: optional(in.IdempotencyKey)}

			t, replayed, err := d.tickets.CreateIdempotent(ctx, me.AccountID, p)
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			// A replay wrote nothing, so there is nothing to announce.
			if !replayed {
				d.feed.publish(ctx, realtime.TicketCreated, me.AccountID, t)
			}

			return asBody(t), nil
		})

	addMCPTool(s, perms, "update_ticket",
		"Edits a ticket and returns it. Send only what changes: title, description (Markdown, replaces the old text), assignee_id "+
			"(an account id, me, or none), epic_id (the parent epic, or none) and label_ids (the complete set). To change status use "+
			"move_ticket. The write is conditional on the version just read, so a concurrent edit is refused with ticket_conflict "+
			"carrying the current ticket. The change appears in the activity list and live in open browsers.",
		auth.TicketWrite, true, func(ctx context.Context, me auth.Identity, in updateTicketArgs) (any, error) {
			// The service replaces wholesale (PUT), so read what is there and send it back with
			// the changes. IfVersion is the version just read: an edit that lands in between is
			// a conflict the agent sees, not a silent overwrite.
			cur, err := d.tickets.Get(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			p := ticket.UpdateParams{ID: cur.ID, Title: cur.Title, Description: cur.Description,
				StatusID: cur.StatusID, AssigneeID: cur.AssigneeID, IfVersion: &cur.Version}

			if in.Title != nil {
				p.Title = *in.Title
			}
			if in.Description != nil {
				p.Description = *in.Description
			}
			if in.AssigneeID != nil {
				p.AssigneeID = nullable(resolveMe(me, *in.AssigneeID))
			}
			if in.EpicID != nil {
				p.Epic = ticket.Ref{Set: true, ID: nullable(*in.EpicID)}
			}
			if in.LabelIDs != nil {
				p.LabelIDs = append([]string{}, *in.LabelIDs...)
			}

			t, err := d.tickets.Update(ctx, me.AccountID, p)
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			d.feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)

			return asBody(t), nil
		})

	addMCPTool(s, perms, "move_ticket",
		"Moves a ticket to another status (a board column) and returns it. Give status_id, or status by name such as In Progress "+
			"(matched among the ticket's team's statuses, ignoring case; list_statuses shows them). Optionally place it before_id or "+
			"after_id another ticket in that column; otherwise it goes to the end. The move appears in the activity list and live in "+
			"open browsers.",
		auth.TicketWrite, true, func(ctx context.Context, me auth.Identity, in moveTicketArgs) (any, error) {
			if (in.StatusID == "") == (in.Status == "") {
				return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
					"send exactly one of status_id and status", "arguments", "exactly one of status_id and status is required")
			}
			if in.BeforeID != "" && in.AfterID != "" {
				return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
					"send at most one of before_id and after_id", "arguments", "before_id and after_id are mutually exclusive")
			}

			statusID := in.StatusID
			if statusID == "" {
				var err error
				if statusID, err = statusByName(ctx, d, me, in.ID, in.Status); err != nil {
					return nil, err
				}
			}

			t, renumbered, err := d.tickets.Move(ctx, me.AccountID, ticket.MoveParams{
				ID: in.ID, StatusID: statusID, BeforeID: optional(in.BeforeID), AfterID: optional(in.AfterID),
			})
			if err != nil {
				return nil, ticketError(ctx, d.accounts, err)
			}

			for _, o := range renumbered {
				d.feed.publish(ctx, realtime.TicketUpdated, me.AccountID, o)
			}
			d.feed.publish(ctx, realtime.TicketUpdated, me.AccountID, t)

			return asBody(t), nil
		})

	addMCPTool(s, perms, "add_comment",
		"Adds a Markdown comment to a ticket or a document (exactly one of ticket_id and document_id) and returns it. Send "+
			"idempotency_key to make a retry safe. It appears in the activity list and live in open browsers.",
		auth.TicketWrite, true, func(ctx context.Context, me auth.Identity, in addCommentArgs) (any, error) {
			c, replayed, err := d.comments.Create(ctx, me.AccountID, comment.CreateParams{
				TicketID: optional(in.TicketID), DocumentID: optional(in.DocumentID), Body: in.Body,
				IdempotencyKey: optional(in.IdempotencyKey),
			})
			if err != nil {
				return nil, commentError(err)
			}

			if !replayed {
				d.feed.publishComment(realtime.CommentCreated, me.AccountID, c)
			}

			return asCommentBody(c), nil
		})

	addMCPTool(s, perms, "save_document",
		"Edits a document, or creates one when id is omitted, and returns it (see get_document for the shape). To edit, send id and "+
			"only what changes: title, content (the Markdown text of a normal document) or fields (aspect field id to Markdown; others are "+
			"kept). The write is conditional on if_version, default the version just read, so a concurrent edit is refused with "+
			"document_conflict carrying the current copy. To create, send title and content, and optionally project_id or team_id.",
		auth.TicketWrite, true, func(ctx context.Context, me auth.Identity, in saveDocumentArgs) (any, error) {
			if in.ID == "" {
				body := map[string]any{}
				if in.Content != nil {
					body["content"] = *in.Content
				}
				for k, v := range in.Fields {
					body[k] = v
				}

				doc, replayed, err := d.documents.WriteIdempotent(ctx, document.SaveParams{
					IdempotencyKey: optional(in.IdempotencyKey), AccountID: me.AccountID, Title: deref(in.Title),
					ProjectID: in.ProjectID, TeamID: in.TeamID, Body: body,
				})
				if err != nil {
					return nil, documentError(err)
				}
				if !replayed {
					d.feed.publishDocument(realtime.DocumentCreated, me.AccountID, doc)
				}

				return asDocumentBody(doc), nil
			}

			cur, err := d.documents.Get(ctx, me.AccountID, in.ID)
			if err != nil {
				return nil, documentError(err)
			}

			// PUT replaces wholesale, so start from the stored body and overlay what was sent.
			body := make(map[string]any, len(cur.Body)+len(in.Fields)+1)
			for k, v := range cur.Body {
				body[k] = v
			}
			if in.Content != nil {
				body["content"] = *in.Content
			}
			for k, v := range in.Fields {
				body[k] = v
			}

			version := cur.Version
			if in.IfVersion > 0 {
				version = in.IfVersion
			}

			p := document.SaveParams{AccountID: me.AccountID, ID: cur.ID, Title: cur.Title, Type: cur.Type,
				AspectTypeID: strOrEmpty(cur.AspectTypeID), Body: body, IfVersion: &version}
			if in.Title != nil {
				p.Title = *in.Title
			}

			doc, err := d.documents.Write(ctx, p)
			if err != nil {
				return nil, documentError(err)
			}

			d.feed.publishDocument(realtime.DocumentUpdated, me.AccountID, doc)
			publishStaledReviews(ctx, d.comments, d.feed, me.AccountID, doc)

			return asDocumentBody(doc), nil
		})

	addMCPTool(s, perms, "create_team",
		"Creates a team in your workspace and returns it: {id, name, statuses}. Only name is required. The team comes with the "+
			"same starter statuses and aspect types as one made in the app. Fails with team_name_taken when the workspace already "+
			"has a team of that name. Open browsers show the new team live.",
		auth.TeamWrite, true, func(ctx context.Context, me auth.Identity, in createTeamArgs) (any, error) {
			// The caller's workspace, resolved as the change feed resolves it, so the id is not
			// an argument an agent could get wrong.
			profile, err := d.accounts.Profile(ctx, me.AccountID)
			if err != nil {
				return nil, err
			}
			if profile.WorkspaceID == "" {
				return nil, NewError(http.StatusForbidden, CodeNoWorkspace, "this account is not in a workspace")
			}

			t, err := d.teams.Create(ctx, me.AccountID, team.CreateParams{WorkspaceID: profile.WorkspaceID, Name: in.Name})
			if err != nil {
				return nil, teamError(err)
			}

			d.feed.publishTeam(t.WorkspaceID, me.AccountID, t)

			return asTeamBody(t), nil
		})

	addMCPTool(s, perms, "create_project",
		"Creates a project in a team and returns it: {id, team_id, name, board_id, columns}. Needs team_id (from list_teams) and name. "+
			"The project comes with a board that has a column per status the team has. Open browsers show the new project live.",
		auth.ProjectWrite, true, func(ctx context.Context, me auth.Identity, in createProjectArgs) (any, error) {
			p, err := d.projects.Create(ctx, me.AccountID, project.CreateParams{TeamID: in.TeamID, Name: in.Name})
			if err != nil {
				return nil, projectError(err)
			}

			if ws, err := d.projects.WorkspaceID(ctx, p.ID); err == nil {
				d.feed.publishProject(ws, me.AccountID, p)
			}

			return asProjectBody(p), nil
		})
}

// statusByName finds a status of the ticket's team by name. It goes ticket, project, team -
// the same chain the REST write validates a status_id along - and says what the choices were
// when none matches, since the agent's next move is to pick one.
func statusByName(ctx context.Context, d *mcpDeps, me auth.Identity, ticketID, name string) (string, error) {
	t, err := d.tickets.Get(ctx, me.AccountID, ticketID)
	if err != nil {
		return "", ticketError(ctx, d.accounts, err)
	}

	p, err := d.projects.Get(ctx, me.AccountID, t.ProjectID)
	if err != nil {
		return "", projectError(err)
	}

	tm, err := d.teams.Get(ctx, me.AccountID, p.TeamID)
	if err != nil {
		return "", teamError(err)
	}

	var names []string
	for _, st := range tm.Statuses {
		if strings.EqualFold(st.Name, strings.TrimSpace(name)) {
			return st.ID, nil
		}
		names = append(names, st.Name)
	}

	return "", newFieldError(CodeStatusNotFound, fmt.Sprintf("no status named %q on this ticket's team; its statuses are: %s", name, strings.Join(names, ", ")),
		"arguments.status", "no such status for this ticket's team")
}

// optional is the pointer a service takes for an optional id: nil when none was sent.
func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// nullable reads the tools' spelling of "clear it": the word none, or an empty string.
func nullable(s string) *string {
	if s == "" || strings.EqualFold(s, "none") {
		return nil
	}

	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

/*
╔═ realtime.go ═════════════════════════════════════════════════════════════════════════
║  http handlers · change feed
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      WSPath                 const
║      CodeOriginNotAllowed   const
║      CodeNoWorkspace        const
║      rawRoute              struct
║      ticketFeed            struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewMux         →  rawRoutes · GET /api/v1/ws
║      registerTickets  →  ticketFeed.publish after each committed write
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/board"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// WSPath is the change feed. Not an OpenAPI operation - huma has no model for an
// upgrade - so TestRawRoutesCarryTheVersionPrefix stands in for the document-driven
// guard, and docs/realtime.md for the document.
const WSPath = V1 + "/ws"

// Two codes the upgrade can be refused with that the derived ones do not say. A
// missing or invalid credential is the derived `unauthorized`, as everywhere.
const (
	// CodeOriginNotAllowed: the Origin header names a different host than the request.
	CodeOriginNotAllowed = "origin_not_allowed"
	// CodeNoWorkspace: a valid credential whose account belongs to no workspace, so
	// there is nothing for it to watch.
	CodeNoWorkspace = "no_workspace"
)

// feedTimeout bounds the one lookup a publish makes. Generous, because it runs on the
// request's goroutine after the write succeeded and a slow answer delays only that
// response; it is bounded so a stalled database cannot hold the response forever.
const feedTimeout = 5 * time.Second

/*
┏━ rawRoute ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  a handler registered on the mux directly, not through huma
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Method     string
┃      Path       string          composes V1
┃      Handler    http.Handler
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      rawRoutes                     one per process
*/

// Kept as a list, and registered from it, so a guard can walk it. A raw route is
// invisible to the OpenAPI document that TestEveryRouteCarriesTheVersionPrefix and
// TestEveryOperationDeclaresItsAuth read; this list is how they are seen at all. A
// future raw route under V1 goes here, and inherits both tests.
type rawRoute struct {
	Method  string
	Path    string
	Handler http.Handler
}

func rawRoutes(authSvc *auth.Service, accounts *account.Service, hub *realtime.Hub) []rawRoute {
	return []rawRoute{
		{Method: http.MethodGet, Path: WSPath, Handler: feedHandler(authSvc, accounts, hub)},
	}
}

/*
┌─ api ───────────────────────────────────────────
│  authenticates the upgrade, then hands it to the hub
├─ in ────────────────────────────────────────────
│      authSvc    *auth.Service
│      accounts   *account.Service    names the caller's workspace
│      hub        *realtime.Hub
├─ out ───────────────────────────────────────────
│      http.Handler    401 · 403 before the upgrade
├─ example ───────────────────────────────────────
│      no cookie  →  401 unauthorized, no upgrade
*/

// Every refusal happens before the upgrade, in the ordinary error envelope, so a
// browser sees a failed handshake with a status it can read rather than an open socket
// that closes. Origin is checked before the credential: it costs nothing, and a
// cross-origin page that has no business here should not be able to make the server
// touch the database on its behalf.
//
// Authentication is auth.Authenticate, the call the huma middleware makes, so a
// session cookie and a bearer token both work and a stale cookie does not beat a
// token. A browser cannot set headers on a WebSocket, which is why the cookie is the
// path that matters: it travels because the page and the API share an origin.
//
// The workspace is resolved here, once. It is `me`'s answer - account.Profile - so a
// connection watches exactly the workspace the client was told it is in. A session that
// ends, or a membership that changes, is not noticed until the next connect (ADR 0005).
func feedHandler(authSvc *auth.Service, accounts *account.Service, hub *realtime.Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeError(w, NewError(http.StatusForbidden, CodeOriginNotAllowed,
				"the Origin of this request is not the host it was sent to"))
			return
		}

		var cookie string
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			cookie = c.Value
		}

		id, err := authSvc.Authenticate(r.Context(), r.Header.Get("Authorization"), cookie)
		if err != nil {
			writeError(w, NewError(http.StatusUnauthorized, defaultCode(http.StatusUnauthorized),
				"this endpoint requires a credential"))
			return
		}

		profile, err := accounts.Profile(r.Context(), id.AccountID)
		if err != nil && !errors.Is(err, account.ErrNotFound) {
			log.Printf("realtime: resolve workspace for %s: %v", id.AccountID, err)
			writeError(w, NewError(http.StatusInternalServerError, defaultCode(http.StatusInternalServerError),
				"unexpected error occurred"))
			return
		}
		if profile.WorkspaceID == "" {
			writeError(w, NewError(http.StatusForbidden, CodeNoWorkspace,
				"this account belongs to no workspace, so there is nothing to watch"))
			return
		}

		switch err := hub.Serve(w, r, profile.WorkspaceID); {
		case err == nil:
		case errors.Is(err, realtime.ErrClosed):
			writeError(w, NewError(http.StatusServiceUnavailable, defaultCode(http.StatusServiceUnavailable),
				"the server is shutting down"))
		default:
			// Accept has already answered a request that was not a valid upgrade.
			log.Printf("realtime: serve: %v", err)
		}
	})
}

/*
┌─ api ───────────────────────────────────────────
│  the Origin rule: absent, or the same host
├─ in ────────────────────────────────────────────
│      r    *http.Request
├─ out ───────────────────────────────────────────
│      bool
├─ example ───────────────────────────────────────
│      Origin https://evil.example, Host app.local  →  false
*/

// Absent is allowed because a non-browser client - a script holding a token - sends no
// Origin, and the check exists for browsers: a page on another site can open a socket
// to this one, carrying the visitor's cookies if SameSite permitted. Host only, not
// scheme, which is what the library's own default does as well. "null", which a
// sandboxed frame sends, has no host and is refused.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}

	return strings.EqualFold(u.Host, r.Host)
}

/*
┏━ ticketFeed ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  publishes a committed ticket write to the hub
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      hub         *realtime.Hub        nil: publishes nothing
┃      projects    *project.Service     says which workspace
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      publish(ctx, type, actorID, ticket.Ticket)
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewHumaAPI                    one per process
*/

// The publish call lives here, in the API layer, because the event's ticket is the REST
// body: asBody is the one mapping from a ticket to its JSON, and internal/realtime
// cannot import this package. Putting the call beside the handler means the shape on
// the socket and the shape in a response are the same function's output.
type ticketFeed struct {
	hub      *realtime.Hub
	projects *project.Service
}

/*
┌─ ticketFeed ────────────────────────────────────
│  tells the ticket's workspace what just changed
├─ in ────────────────────────────────────────────
│      ctx        context.Context
│      kind       string               a realtime.Ticket* type
│      actorID    string
│      t          ticket.Ticket        as the write returned it
├─ example ───────────────────────────────────────
│      archived at version 5  →  ticket.archived, version 5
*/

// Called only after the service call has returned, which is after its commit: an event
// for a write that rolled back would be a lie no re-list could correct, because the
// client would have already applied it.
//
// Best-effort end to end. The workspace is looked up on a context that survives the
// request being cancelled - the write has happened either way - and a failure is logged
// and swallowed: the write succeeded, and a watcher that missed the event re-lists.
func (f ticketFeed) publish(ctx context.Context, kind, actorID string, t ticket.Ticket) {
	if f.hub == nil || f.projects == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), feedTimeout)
	defer cancel()

	workspace, err := f.projects.WorkspaceID(ctx, t.ProjectID)
	if err != nil {
		log.Printf("realtime: no workspace for project %s, %s on ticket %s not published: %v",
			t.ProjectID, kind, t.ID, err)
		return
	}

	f.hub.Publish(workspace, realtime.Event{
		Type:     kind,
		TicketID: t.ID,
		Version:  t.Version,
		ActorID:  actorID,
		At:       t.UpdatedAt.UTC(),
		Ticket:   asBody(t),
	})
}

/*
┌─ ticketFeed ────────────────────────────────────
│  tells a workspace a team's status changed
├─ in ────────────────────────────────────────────
│      workspaceID    string               the team's, already known to the caller
│      kind           string               a realtime.Status* type
│      actorID        string
│      teamID         string
│      st             team.Status          as written; as it was, for a delete
├─ example ───────────────────────────────────────
│      rename  →  status.updated, status.name the new name
*/

// LAM-83. Same rules as publish: after the commit, best-effort, nothing returned. The
// workspace comes from the caller because the status service already resolved it while
// proving the caller could reach the team.
func (f ticketFeed) publishStatus(workspaceID, kind, actorID, teamID string, st team.Status) {
	if f.hub == nil {
		return
	}

	f.hub.Publish(workspaceID, realtime.Event{
		Type:     kind,
		ActorID:  actorID,
		At:       time.Now().UTC(),
		TeamID:   teamID,
		StatusID: st.ID,
		Status:   asStatusBody(st),
	})
}

// publishMoved announces tickets a status delete re-pointed, one ticket.updated each,
// exactly as an edit would be announced.
func (f ticketFeed) publishMoved(workspaceID, actorID string, moved []ticket.Ticket) {
	if f.hub == nil {
		return
	}

	for _, t := range moved {
		f.hub.Publish(workspaceID, realtime.Event{
			Type:     realtime.TicketUpdated,
			TicketID: t.ID,
			Version:  t.Version,
			ActorID:  actorID,
			At:       t.UpdatedAt.UTC(),
			Ticket:   asBody(t),
		})
	}
}

// publishBoard announces a board as the REST body: board.created, board.updated, and
// board.deleted (the board as it was). LAM-85. After the commit, best-effort, as above.
func (f ticketFeed) publishBoard(workspaceID, kind, actorID string, b board.Board) {
	if f.hub == nil {
		return
	}

	f.hub.Publish(workspaceID, realtime.Event{
		Type:      kind,
		ActorID:   actorID,
		At:        time.Now().UTC(),
		ProjectID: b.ProjectID,
		BoardID:   b.ID,
		Board:     asBoardBody(b),
	})
}

// publishColumn announces a column write as board.column.updated: the column and the
// whole board it is on, so a client can apply either.
func (f ticketFeed) publishColumn(workspaceID, actorID string, res board.Result) {
	if f.hub == nil {
		return
	}

	f.hub.Publish(workspaceID, realtime.Event{
		Type:      realtime.BoardColumnUpdated,
		ActorID:   actorID,
		At:        time.Now().UTC(),
		ProjectID: res.Board.ProjectID,
		BoardID:   res.Board.ID,
		Board:     asBoardBody(res.Board),
		ColumnID:  res.Column.ID,
		Column:    asColumnBody(res.Column),
	})
}

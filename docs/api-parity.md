# API parity: what the UI does, and how a token does it too (LAM-123)

The rule: **anything a person can do in the app, a script holding an API token can do over
HTTP**, with these two exceptions that are deliberate: token management and the zip export are
session-only (a leaked token must not mint more tokens or walk away with the workspace).

## How this was checked

Every call the frontend makes goes through the typed client generated from `api/openapi.json`
(`src/api/client.ts`), plus two raw ones (the attachment upload, the change-feed socket). The
audit extracted every `api.<METHOD>('<path>')` literal under `LaminarFlow-Frontend/src` and
compared the set with the operations in `api/openapi.json`, and read the permission each operation
declares (`needs(auth.X)` / `Permission:` in `internal/api/*.go`; scopes are the names in
`internal/auth/permission.go`). Result: **every frontend call resolves to an operation that
exists and accepts a Bearer token** (except the two session-only ones below, which the UI uses
from a session by design). No UI action depends on a cookie-only route.

The quickstart in the README (`scripts/agent-quickstart.sh`, run in CI against a fresh database)
exercises the central path with only a token.

## Matrix

Scope column: the permission a token needs. `T` = `ticket:`, `A` = `aspect:`, `P` = `project:`,
`M` = `team:`, `S` = `setting:`. Ops marked `idem` take `Idempotency-Key`; `v` take `If-Match`.

| UI action | Frontend | Operation | Scope |
| --- | --- | --- | --- |
| Sign up / sign in / out | `features/auth` | `POST /auth/register`, `/auth/login`, `/auth/logout` | none (public / session) |
| Who am I, what may I do | `features/auth/session.ts` | `GET /auth/me` | any credential |
| Create / list / revoke API token | `features/tokens`, `routes/tokens` | `POST /auth/tokens`, `POST /auth/tokens/list`, `DELETE /auth/tokens/{id}` | **session only** |
| Create team | `sync/store.ts` | `POST /teams` | M write |
| List teams | `sync/remote.ts` | `POST /teams/list`, `GET /teams/{id}` | M read |
| Create project | `sync/store.ts` | `POST /projects` | P write |
| List projects | `sync/entities/*` | `POST /projects/list`, `GET /projects/{id}` | P read |
| Add / rename / recolour / delete / reorder statuses | `sync/entities/status.ts` | `POST /teams/{id}/statuses`, `PUT` and `DELETE .../{status_id}`, `POST .../reorder` | M write |
| List members (assignee picker) | `features/members` | `POST /members/list` | M read |
| Create ticket (also "duplicate") | `sync/entities/ticket.ts` | `POST /tickets` (idem) | T write |
| List / filter / sort tickets | `listView/serverRun.ts`, `sync` | `POST /tickets/list`, `GET /tickets/{id}` | T read |
| Edit ticket fields, status, assignee, labels, epic, milestone | `sync/entities/ticket.ts` | `PUT /tickets/{id}` (v) | T write |
| Move on board / reorder | `sync/entities/ticket.ts` | `POST /tickets/{id}/move` | T write |
| Archive / restore ticket | `sync/entities/ticket.ts` | `DELETE /tickets/{id}`, `POST /tickets/{id}/unarchive` (v) | T write |
| Add / remove ticket in sprint | `sync/entities/ticket.ts` | `POST /sprints/{id}/tickets`, `DELETE /sprints/{id}/tickets/{ticket_id}` | T write |
| Create / list / edit / archive epic | `sync/entities/hierarchy.ts` | `POST /epics`, `POST /epics/list`, `GET`, `PUT`, `DELETE /epics/{id}` | T write / read |
| Same for sprints and milestones | `sync/entities/hierarchy.ts` | `/sprints`, `/milestones` (same shapes) | T write / read |
| Create / rename / delete labels | `sync/entities/label.ts` | `POST /labels`, `POST /labels/list`, `GET`, `PUT`, `DELETE /labels/{id}` | T write / read |
| Boards: create, rename, delete, column rename/visibility, column statuses, reorder | `sync/entities/board.ts` | `POST /projects/{id}/boards`, `POST /boards/list`, `GET`, `PUT`, `DELETE /boards/{id}`, `PUT .../columns/{id}`, `PUT .../columns/{id}/statuses`, `POST .../columns/reorder` | T write / read |
| Add / delete a board column | none (API only) | `POST /boards/{id}/columns`, `DELETE .../columns/{column_id}` | T write |
| Saved views: create, list, edit, delete | `features/tickets/savedViews` | `POST /saved-views`, `POST /saved-views/list`, `GET`, `PUT`, `DELETE /saved-views/{id}` | T write / read |
| Create / edit / archive / restore document | `sync/entities/document.ts`, `features/documents` | `POST /documents` (idem), `PUT /documents/{id}` (v), `DELETE /documents/{id}`, `POST /documents/{id}/unarchive`, `GET /documents/{id}`, `POST /documents/list` | T write / read |
| Aspect types: create, rename, import, export, fields add/rename/remove/reorder, field usage | `features/aspects/api.ts` | `/aspect-types` (all 10 operations) | A write / read |
| Link / unlink a document and a ticket | `features/links/api.ts` | `POST /tickets/{id}/documents`, `DELETE /tickets/{id}/documents/{document_id}` | T write |
| Read the links from either side | `features/links/api.ts` | `GET /tickets/{id}` (`documents`), `GET /documents/{id}` (`tickets`) | T read |
| Comments: add, edit, delete, list | `features/comments` | `POST /comments` (idem), `PUT`, `DELETE /comments/{id}`, `POST /comments/list` | T write / read |
| Request a review, list requests, accept / change status | `features/review/api.ts` | `POST /review-requests` (idem), `POST /review-requests/list`, `PUT /review-requests/{id}` | T write / read |
| Upload / list / delete attachments, view an image | `features/attachments/api.ts`, `components/editor/images.ts` | `POST /attachments` (idem), `POST /attachments/list`, `DELETE`, `GET /attachments/{id}` | T write / read |
| Activity feed | `features/activity/api.ts` | `POST /activity/list` | T read |
| Search / command palette | `features/search/api.ts` | `POST /search/list` | T read |
| Live updates | `sync/socket.ts` | `GET /ws` (raw route, Bearer or cookie) | any credential, see gap 2 |
| Export everything | none in the UI today | `POST /export` | **session only** (deliberate) |
| Delete my account | none in the UI today | `DELETE /accounts/me` | `account:delete` |
| Settings key/value | none in the UI today | `GET /settings`, `GET`, `PUT`, `DELETE /settings/{key}` | S read / write |
| Health | `api/ping.ts` | `GET /ping` | public |

Read the reverse too: operations the UI does not call (the `GET /x/{id}` of epics, labels,
milestones, sprints, projects, teams and saved views, board column add/delete, settings, export,
account delete) are all reachable by a token, so the API is a superset of the UI.

## Findings

1. **No missing endpoints.** The ticket text (written when the API had tickets only) listed
   documents, comments, review requests, sprints, boards, labels, saved views, epics and
   export as unexposed. All exist now except export, which is session-only on purpose. The
   remaining LAM-123 deliverable was the quickstart, its CI run and this document.
2. **The change feed does not check scopes.** `GET /ws` authenticates a token but never calls
   `auth.Allows`, so any valid token, even one scoped to `setting:read` alone, receives every
   workspace event. Not fixed here: the right rule (one required scope? per-event filtering?)
   is a design call. Follow-up ticket.
3. **Idempotency-Key is on some creates only**: tickets, documents, comments, review requests,
   attachments. Creates of teams, projects, epics, sprints, milestones, labels, boards, saved
   views, aspect types, statuses and token do not take it, so a retry after a lost response
   duplicates. That is LAM-127; left alone here.
4. **No token export.** `POST /export` waits for an `export:read` permission (noted in its
   description). Until then use a session, or `go run ./cmd/export`.
5. **Ticket-document links need no list endpoint**: they are in the ticket (`documents`) and
   document (`tickets`) detail responses, which is also what the UI reads.
6. **Scopes follow the resource family, not the noun**: documents, comments, review requests,
   boards, sprints, epics, labels, saved views, attachments, search and activity all use
   `ticket:read` / `ticket:write`. There is no `document:*` scope; a token that may edit tickets
   may edit documents.

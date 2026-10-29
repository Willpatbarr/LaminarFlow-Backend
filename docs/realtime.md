# Realtime — the change feed

`GET /api/v1/ws` upgrades to a WebSocket and pushes a message whenever a ticket in your
workspace is created, edited, archived or restored. LAM-66. The reasoning is in
[ADR 0005](adr/0005-in-process-change-feed.md); this file is the contract.

It is **not in `openapi.json`**: the OpenAPI document cannot describe an upgrade, so this
page is the only description, and the raw-route tests in `internal/api/realtime_test.go`
(`TestRawRoutesCarryTheVersionPrefix`, `TestEveryRawRouteRefusesAnUnauthenticatedCaller`)
are the only guards on the route. Within v1 it is additive-only like everything else
(ADR 0003): new event types and new members may appear, and a client ignores what it does
not know.

## What it is, and is not

A **hint with the full row attached**, delivered best-effort. It is not a log: there is no
replay, no sequence number and no resume cursor. A client that missed something — it was
offline, the server restarted, it was dropped for being slow — learns of it by **listing
again after it reconnects**. Treat every connection as "list, then apply what arrives".

## Connecting

    GET /api/v1/ws
    Upgrade: websocket

**Authentication** is `auth.Authenticate`, at the handshake, like every other endpoint: the
session cookie (a browser cannot set headers on a WebSocket, so this is the path that
matters) or `Authorization: Bearer lam_…`. The credential is not checked again; a session that
ends keeps its socket until the connection does (ADR 0005).

**Origin.** When the request has an `Origin` header, its host must equal the request's
`Host` header (scheme and port are compared as part of the host; case is ignored). No
`Origin` — a script, `curl`, a test client — is allowed. Behind a reverse proxy the proxy must
pass the browser's `Host` through, or every browser is refused.

| Refusal | Status | `code` | When |
| --- | --- | --- | --- |
| Cross-origin | `403` | `origin_not_allowed` | `Origin` names another host (also `null`, and anything unparseable). Checked first |
| No or bad credential | `401` | `unauthorized` | Nothing, an unknown cookie, an invalid token |
| No workspace | `403` | `no_workspace` | Valid credential, account in no workspace |
| Token without a read scope | `403` | `insufficient_scope` | An API token holding neither `ticket:read` nor `team:read` (LAM-129). Checked after the credential, before the upgrade |
| Not an upgrade | `426` | — | Signed in, but a plain GET |
| Shutting down | `503` | `service_unavailable` | The server is closing |

All but `426` are the ordinary error envelope (`docs/api-errors.md`), and all happen
**before the upgrade completes**, so a browser sees a failed handshake, not a socket that
opens and closes.

**Token scopes (LAM-129).** A session receives every event. A token receives only the events
whose resource its scopes let it read over REST; the rest are not sent to that connection at
all (the `hello` is always sent). The mapping is one table, `eventPermission` in
`internal/api/realtime.go`, and a test fails when an event type has no row:

| Needs | Events |
| --- | --- |
| `ticket:read` | `ticket.*` (including `ticket.document_linked` / `_unlinked`), `document.*`, `comment.*`, `review_request.*`, `activity.created`, `board.*`, `epic.*`, `sprint.*`, `milestone.*`, `label.*` |
| `team:read` | `status.*` |

There is no `document:*` or `board:*` scope: those resources use `ticket:read` over REST
too ([api-parity.md](api-parity.md) finding 6). A new event type must be given its read
permission; an unmapped type is delivered to no token (fail closed). The scopes are
those the token holds when the connection is made; revoking a token does not close an open
socket (see Lifetime above and ADR 0005).

**A token with none of the needed scopes is refused**, `403 insufficient_scope`, before the
upgrade, rather than accepted and left silent: a socket that can only ever deliver `hello`
looks healthy while being useless, and an early refusal tells the client author what is
missing. A token with only `team:read` can connect and receives `status.*` events alone.

**Workspace.** A connection receives the events of the workspace that
`GET /api/v1/auth/me` reports as `workspace_id`, fixed when the connection is made. Another
workspace's connection receives nothing.

## Frames

The server sends **text frames, one JSON object each**. The client sends nothing: a data frame
from the client closes the connection with status 1008.

### `hello`

The first frame on every connection, before any event:

    {"type":"hello"}

An open socket is not a working one — a proxy that accepts the upgrade and buffers shows up as
a connection that never delivers. Once `hello` has arrived the whole path works; it is also the
moment to **list** (events that arrive while you list are applied by version, so the order
does not matter).

### Events

    {
      "type": "ticket.updated",
      "ticket_id": "8b1c…",
      "version": 6,
      "actor_id": "4f0e…",
      "at": "2026-10-13T17:02:11.482913Z",
      "ticket": { …TicketBody… }
    }

| Member | Meaning |
| --- | --- |
| `type` | `ticket.created`, `ticket.updated`, `ticket.archived`, `ticket.unarchived` |
| `ticket_id` | The ticket's id; equals `ticket.id` |
| `version` | The ticket's version **after** this change; equals `ticket.version` |
| `actor_id` | The account that made the change; equals `ticket.updated_by` |
| `at` | The ticket's `updated_at` after the change, UTC |
| `ticket` | The whole ticket, in **exactly the shape of `TicketBody`** (`GET /tickets/{id}`) — same members, same nullability — except for the `$schema` link huma adds to an HTTP response. It is built by the same function, so the two cannot drift |

`ticket` is complete for **every** type, archive included. An archived ticket is otherwise
unreadable (`GET` answers `ticket_not_found`), so the event is the only place a client sees it
at its archived version.

A create replayed with the same `Idempotency-Key` ([api-conventions.md §10](api-conventions.md))
writes nothing and publishes nothing: one `ticket.created` per ticket, however many times the
request is retried. The same holds for `POST /documents` and `POST /comments`: one `document.created` or `comment.created` each, and for every other create (LAM-127): one `epic.created`, `status.created`, `board.created` and so on.

### What a client does with an event

1. **`created`, `updated`, `unarchived`:** upsert `ticket` if `version` is greater than the
   local copy's. Equal or lower: ignore it. That covers your **own write** — the response to a
   `PUT` already told you the new version (`ETag`), so its echo arrives at a version you hold.
2. **`archived`:** remove the ticket locally (if `version` is not older than the local copy).
3. **Any type you do not know:** ignore.

Events from different writers are not ordered: version 3 can arrive before version 2. The rule
above makes that harmless — and a ticket that was archived at 3 and restored at 4 ends up
present at 4 whichever order they arrive in, provided the client keeps a tombstone of the
archive's version for the length of a list, or simply re-lists on doubt.

Only **successful** writes publish. A write refused with `409` or `404` changes nothing and
says nothing.

## Lifetime

* The server **pings every 30 seconds** and closes a connection that has not answered within
  10. A browser answers pings by itself.
* A connection whose client reads too slowly — 64 frames queued, or a write stalled for 10
  seconds — is **closed by the server** (abnormal closure, no close frame). Reconnect and list.
* On server shutdown every connection receives a close frame with status 1001 (going away).
* A client that sees any close or error: reconnect with backoff, wait for `hello`, list.

## Status events (LAM-83)

`status.created`, `status.updated` and `status.deleted` carry `type`, `actor_id`, `at` (server
time), `team_id`, `status_id` and `status` (the whole `StatusBody`; on `deleted`, the status as it
was). They have **no** `ticket_id`, `version` or `ticket`. A reorder sends `status.updated` for
each status whose position changed. A delete that moves tickets also sends one `ticket.updated`
per moved ticket (archived ones included) at their new version. Apply `status.*` by upsert or
remove on `status_id`; there is no version, so re-list on doubt.

## Board events (LAM-85)

`board.created`, `board.updated`, `board.deleted` and `board.column.updated` carry `type`, `actor_id`,
`at` (server time), `project_id`, `board_id` and `board` (the whole `BoardBody`, every column with
its `status_ids`; on `deleted`, the board as it was). `board.column.updated` also carries
`column_id` and `column` (the `ColumnBody`). They have **no** `ticket_id`, `version` or `ticket`.

| Write | Event |
| --- | --- |
| create-board | `board.created` |
| rename-board, reorder-columns, delete-column | `board.updated` (the delete's `board` no longer has the column) |
| add-column, update-column, set-column-statuses | `board.column.updated` |
| delete-board | `board.deleted` |

There is no version: replace the board (or the column) you hold with the one in the event, and re-list on
doubt. A ticket move (`POST /tickets/{id}/move`) is an ordinary `ticket.updated` at the new version,
with the new `status_id` and `position` in `ticket`. When a move had to renumber the target column (no
whole number between two neighbours), every other ticket whose `position` changed is written like an edit
(version + 1) and gets its own `ticket.updated`, sent before the moved ticket's. Apply them by version as
usual; no new event type.

## Hierarchy and label events (LAM-93)

`epic.*`, `sprint.*`, `milestone.*` and `label.*` (each `created`, `updated`, `deleted`) carry `type`,
`actor_id`, `at` (server time) and the resource under its own name, whole, as its REST body (`EpicBody`,
`SprintBody`, `MilestoneBody`, `LabelBody`; on `deleted`, the resource as it was). Epic, sprint and milestone
events also carry `project_id` and `epic_id` / `sprint_id` / `milestone_id`; label events carry `team_id` and
`label_id`. They have **no** `ticket_id`, `version` or `ticket`. There is no version: replace the resource you
hold, and re-list on doubt.

| Write | Event |
| --- | --- |
| create-epic, create-sprint, create-milestone, create-label | `<resource>.created` |
| update-epic, update-sprint, update-milestone, update-label | `<resource>.updated` |
| delete-epic, delete-sprint, delete-milestone, delete-label | `<resource>.deleted` |
| add-ticket-to-sprint, remove-ticket-from-sprint | `ticket.updated` (version + 1), only when membership changed |
| a ticket create or update that carries `epic_id`, `milestone_id` or `label_ids` | the usual `ticket.created` / `ticket.updated`, with those members in `ticket` |

A delete does **not** rewrite the tickets that pointed at the resource, so no `ticket.updated` follows and
their `version` does not move. The database nulls `epic_id` / `milestone_id` and drops the label from
`label_ids`; a client applies the same to the tickets it holds when it sees `epic.deleted`,
`milestone.deleted` or `label.deleted`, and a re-list shows the same. A sprint delete drops the memberships
the same way (read through the `sprint` filter, so a re-list shows them).

## Document events (LAM-87)

`document.created`, `document.updated`, `document.archived` and `document.unarchived` carry
`type`, `document_id`, `version` (the document's version after the change), `actor_id`, `at` (the
document's `updated_at`, UTC) and `document` — the whole document in exactly the shape of
`DocumentBody` (`GET /documents/{id}`). `project_id` and `team_id` are set when the document is
filed under one. They have **no** `ticket_id` or `ticket`.

Apply them as ticket events are applied: upsert `document` if `version` is newer than the local
copy's, remove it on `archived`. `document` is complete on `archived` — the archived document
(`archived_at` set) is otherwise unreadable. Only successful writes publish.

## Ticket-document link events (LAM-111)

`ticket.document_linked` and `ticket.document_unlinked` carry `type`, `ticket_id`, `document_id`,
`project_id` (the ticket's), `actor_id` and `at` (server time). They have **no** `version`, `ticket` or
`document`: a link is neither's version. Apply one by adding or removing `document_id` in the linked
documents you hold for `ticket_id` (and `ticket_id` in the document's linked tickets); re-fetch
`GET /tickets/{id}` or `GET /documents/{id}` on doubt.

Only a link that actually changed publishes: a repeated `POST` or `DELETE` (both 204) is silent. Archiving
or unarchiving either end publishes its own `*.archived` / `*.unarchived` and leaves the link alone.

## Comment events (LAM-99)

`comment.created`, `comment.updated` and `comment.deleted` carry `type`, `comment_id`, `actor_id`, `at`,
`comment` — the whole comment in exactly the shape of `CommentBody` — and the comment's parent as
`ticket_id` or `document_id` (exactly one). On `comment.deleted`, `comment` is the comment as it was and
`at` is server time; otherwise `at` is the comment's `updated_at`. They have **no** `version` (a comment
has none) and no `ticket` or `document`: the parent did not change. **Dispatch on `type`, not on the
presence of `ticket_id`** - a `comment.*` event is not a ticket event.

Apply one to the thread you hold for that parent: add `comment` on `created` (skip it if its `id` is
already there - your own write arrives as an event too), replace by `id` on `updated`, remove by `id`
on `deleted`. There is no ordering token, so on doubt re-list with `POST /comments/list`. Only a
successful write publishes; a create replayed with its `Idempotency-Key` does not.

Comments on a ticket or document that is later archived publish nothing: the parent's own
`*.archived` event is the signal to hide the thread, and `*.unarchived` brings it back.

## Review request events (LAM-113)

A review request is a comment (see [review-requests.md](review-requests.md)), and has two events of its own:

* `review_request.created` - `POST /review-requests`.
* `review_request.updated` - `PUT /review-requests/{id}` that changed the `review_status`, and also an approved
  request going stale (LAM-122), and a request re-anchored to a field when its document became an aspect
  (LAM-136; `field_id` and the offsets are in the body): published after the `PUT /documents/{id}` that sent it back to `pending` commits,
  with `stale_reason` set in the body and the document write's account as `actor_id`. One event per request, after
  the `document.updated` of that write. Setting the status
  it already has publishes nothing, nor does a refused write.

Both carry `type`, `comment_id` (the request's id), `document_id`, `actor_id`, `at` (the request's `updated_at`, which a status change moves; `edited_at` is the body edit alone)
and `review_request` - the whole request in exactly the shape of `ReviewRequestBody`, `reviewers` included. Like
`comment.*` they have **no** `version`, no `ticket` and no `document`; dispatch on `type`.

Apply one to the review requests you hold for that document: upsert by `id`. The request is also a comment
in the document's thread, so a client that keeps both lists keys them by `id`. A **body edit or a delete** goes
through the comment endpoints and publishes `comment.updated` / `comment.deleted`, whose `comment.review_status`
is non-null for a review request: remove by `id` from both lists on `comment.deleted`. A `comment.updated` carries `edited_at` (a body edit) but does
not carry `reviewers` or `anchor_text`; keep what you hold. On doubt, re-list with `POST /review-requests/list`.

## Activity events (LAM-89)

`activity.created` announces one entry of the activity log (`POST /activity/list`, [activity.md](activity.md)). It
carries `type`, `activity_id`, `actor_id`, `at` (the entry's `created_at`), `activity` - the whole entry in exactly
the shape of `ActivityBody` - and the entity it is about as `ticket_id` or `document_id` (exactly one). It has **no**
`version`, `ticket` or `document`: the entity's own event says what it looks like now.

It is published right after the `ticket.*` or `document.*` event of the write that made the entry, one event per entry
(a write that changes a title and a status makes two), and only after the commit. Entries are immutable, so there is no
`activity.updated` or `activity.deleted`. Apply one to the history you hold for that entity: add `activity` at the top
(skip it if its `id` is already there). A ticket's status change from a status delete, and a board drop into another
column, each publish theirs too. On doubt, re-list.

## Not here yet

No per-connection filtering (a project, a status), no events for resources other than tickets, statuses, boards, epics, sprints, milestones, labels, documents, comments, review requests and activity,
no client-to-server messages, no resume. Each is additive.

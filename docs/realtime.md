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
| Not an upgrade | `426` | — | Signed in, but a plain GET |
| Shutting down | `503` | `service_unavailable` | The server is closing |

All but `426` are the ordinary error envelope (`docs/api-errors.md`), and all happen
**before the upgrade completes**, so a browser sees a failed handshake, not a socket that
opens and closes.

**Scope.** A connection receives the events of tickets in the workspace that
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

## Not here yet

No per-connection filtering (a project, a status), no events for resources other than tickets,
no client-to-server messages, no resume. Each is additive.

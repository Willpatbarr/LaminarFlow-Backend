# 5. The change feed is one in-process hub — best-effort delivery, re-list on reconnect, no broker

Status: Accepted — 2026-10-13 (LAM-66)

## Context

The browser client keeps tickets in IndexedDB and edits them locally. Without a push channel
it learns of other people's changes only by polling. Sync & Real-Time notes §2 chose
WebSockets over polling; nothing served one.

Three facts shape what is built:

* **The client is already tolerant.** On each event it upserts the ticket if the event's
  `version` is newer than its copy, removes it on archive, and after any reconnect re-lists to
  close gaps (LAM-64, LAM-65). A missed event costs a stale row until the next list, not
  corruption — `version` makes every apply idempotent and order-insensitive.
* **The deployment is one process.** One binary, one Postgres, one host (ADR 0002). There is
  no second instance to share a connection set with.
* **The source of truth is Postgres.** A change is real when its transaction commits. The feed
  describes it; it is never the only record.

## Decision

### 1. One hub, in this process, no broker

`internal/realtime.Hub` holds the open connections, grouped by workspace, behind one mutex.
`Publish(workspace, event)` encodes once and offers the frame to each connection's queue. There
is no Redis, no NATS and no Postgres `LISTEN/NOTIFY`: each would be a second moving part to
deploy, monitor and keep consistent, to serve a topology that does not exist.

### 2. Delivery is best-effort, and the contract says so

An event is published once the write's transaction has committed — never
before, so a rolled-back write is never announced — and it is **not** retried, persisted or
replayed. It can be lost if: the process stops between commit and publish; the workspace lookup
fails (logged); the connection's queue is full (the connection is dropped, below); or the
client was not connected. None of those is repaired by the server.

The repair is the client's, and it already exists: **after any reconnect, list again.** The
`hello` frame the server sends first on every connection tells a client the socket works, and
therefore when to start that list. Events carry no sequence number or resume cursor, deliberately:
a cursor would need the server to remember what it sent to whom, which is exactly the state
best-effort exists to avoid, and `version` on the ticket already answers "is this news".

Events are not ordered across writers. Two commits can publish in either order, so version 3 may
reach a client before version 2. The client's rule — apply only if newer — makes that harmless,
and it is why every event carries the whole ticket.

### 3. A slow client is dropped, not waited for

Each connection has a bounded send queue (64 frames). `Publish` never blocks: if a queue is full
the connection is closed and removed, and the client reconnects and re-lists. One stalled tab
cannot hold up a ticket write, which is the request that happened to publish, nor delay any other
connection. A write that cannot finish in 10 seconds drops the connection by the same path.

### 4. Dead connections are reaped by ping

The server pings every 30 seconds and closes a connection that has not answered within 10. A
laptop lid closed on a train sends no FIN; without this the hub would hold the socket, its queue
and two goroutines until the kernel's keepalive noticed, which is hours.

### 5. Scope: the workspace, resolved once at connect

A connection receives the events of tickets whose project is in the workspace that
`GET /api/v1/auth/me` reports for the caller (`account.WorkspaceOf` — the oldest membership,
ADR 0004). It is looked up once, at the handshake, and held for the life of the connection. The
ticket's workspace is looked up per event, from its project, after the commit.

### 6. Authentication at the handshake only

The upgrade is refused with `401` before it completes unless `auth.Authenticate` accepts the
request: the session cookie (the browser path — a WebSocket cannot set headers) or a bearer
token. `Origin`, when present, must have the host of the request or the upgrade is refused with
`403`; an absent `Origin` (a script) is allowed. This is the same-origin deployment (LAM-28)
enforced where it matters: a page on another site can open a socket to any host.

### 7. Library: `github.com/coder/websocket` v1.8.15

Minimal, context-aware, maintained, and with a client in the same module for the tests.
`gorilla/websocket` was the fallback and was not needed: the module resolved inside the Docker
toolchain. One defect in v1.8.15 is worked around rather than accepted: `Conn.CloseRead`, which
is the library's way of reading and discarding, deadlocks for 15 seconds when a client sends a
data frame (it calls `Close` from the goroutine `Close` then waits on). `Hub.Serve` runs its own
reader instead, which also lets the hub close a connection with a proper close frame on
shutdown — cancelling a context the library is reading under would sever the socket instead.

## Consequences

* **The hub is per process.** Run two instances behind a load balancer and a write handled by one
  is never announced to the sockets held by the other. Nothing detects this. The change needed
  at that point is small and was kept possible on purpose: publish through a bus every instance
  subscribes to (Postgres `LISTEN/NOTIFY` first — it is already there, and the payload is a
  ticket id and version; the instance that receives it reads the row and calls the same
  `Publish`). Because delivery was already best-effort and idempotent by version, a bus that
  duplicates or reorders events needs no new client logic. Publishing from the committing
  request, as now, becomes publishing from a notification.
* **A logged-out or expired session keeps its socket** until the connection ends. Authentication
  is checked at the handshake and never again. A user who logs out, whose session expires, or
  whose account is deleted continues to receive their workspace's ticket events until they, or
  a deploy, or a ping failure, end the connection. Revocation would need the hub to know which
  session each connection holds (`auth.Identity.SessionID` is available) and a way for log-out
  and expiry to reach it. Not built: the data is no more than the person could have listed a
  moment earlier, and the cost is a coupling between `auth` and the hub that today does not
  exist. Record it as known.
* **Likewise, the workspace is not refreshed.** A membership change takes effect on the next
  connection.
* **No cap on connections** per account or in total. A browser that reconnects in a loop is
  limited only by its own backoff.
* **Shutdown is the hub's job.** `http.Server.Shutdown` does not wait for hijacked connections, so
  `main` closes the hub first, which sends every client a going-away close frame and returns only
  when every connection goroutine has ended. A client treats any close as "reconnect and
  re-list".
* **There is no request timeout, write timeout or response-buffering middleware in front of the
  mux** and the feed relies on that: a server-wide `WriteTimeout` or `http.TimeoutHandler` would
  end every socket. If one is ever added, this route must be carved out. The same goes for a
  reverse proxy, which must pass `Upgrade`, keep the `Host` header the browser used (the Origin
  check compares it), and not idle-time-out a connection quieter than the 30-second ping.
* **The route is not in `openapi.json`.** huma has no model for an upgrade. The raw-route tests in
  `internal/api` stand in for the OpenAPI-driven guards, and `docs/realtime.md` is the contract.

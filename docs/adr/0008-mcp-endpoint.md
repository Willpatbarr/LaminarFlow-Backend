# 8. An MCP endpoint in the backend — stateless, token-authenticated, tools filtered by scope, writes through the REST write path

Status: Accepted — 2026-11-02 (LAM-197)

## Context

A Claude Code session should be able to work LaminarFlow tickets the way it works Linear's:
find them, read them, move them, comment, edit their documents. Claude Code reaches such tools
over the Model Context Protocol (MCP), and for a remote server it speaks Streamable HTTP with a
header for credentials.

The API already has what an agent needs: scoped `lam_...` tokens (E-LAM-0012), idempotent creates,
`If-Match` on updates, and a write path that records activity and tells the change feed. The
risk is a second, parallel way in that drifts from the first.

## Decision

### 1. Built into the backend, at `/mcp`, outside `/api/v1`

The official Go SDK (`github.com/modelcontextprotocol/go-sdk`) serves it from the same process,
registered in `rawRoutes` beside the WebSocket feed: huma cannot describe it, so it is a raw
route and the OpenAPI document does not change. It is not under `/api/v1` because MCP versions
itself in its handshake and the URL is the one a person types into `claude mcp add`; the
raw-route version guard names it as the one exception.

### 2. Stateless Streamable HTTP, plain JSON responses

Every POST stands alone: no `Mcp-Session-Id`, so nothing to lose on a restart and nothing to
share between instances. The tools are request/response, so there is no server-to-client
traffic to hold a stream open for; GET and DELETE answer 405. Responses are JSON rather than
an event stream, which every client reads and `curl` can too.

### 3. Authentication is `auth.Authenticate`, before the SDK sees anything

The same call the huma middleware and the feed make. No valid credential is a 401 in the
ordinary error envelope, not a JSON-RPC error. A bearer token is the intended path; a session
cookie works because `Authenticate` accepts it, so the Origin check the feed uses applies here
too. OAuth, which a claude.ai custom connector would need, is out of scope.

### 4. A tool needs the scope its REST endpoint needs

`tools/list` shows a caller only the tools its scopes allow, so a read-only token never sees a
write tool. `tools/call` checks again inside every tool: naming a tool you were not shown is a
tool error with code `insufficient_scope`, not a crash and not "unknown tool". Tool errors carry
the REST API's stable `code`, plus `current` on a 409, so an agent branches on codes.

### 5. Writes take the REST write path

Tools call the same services as the handlers (`internal/ticket`, `comment`, `document`, ...)
with the same instances, so validation, activity entries, idempotency and `If-Match` are the
service's. Announcing the change on the feed is the handler's job, after the commit; the tools
make the same `ticketFeed` calls, and the one non-trivial piece (approvals made stale by a
document write) was extracted and shared (`publishStaledReviews`) so the two cannot differ.
An agent's change is therefore indistinguishable from anyone else's: in the activity list, and
live in open browsers.

`update_ticket` and `save_document` are read-modify-write over the service's replace-wholesale
update, conditional on the version just read, so a concurrent edit is a visible conflict and
not a silent overwrite.

### 6. A curated set of tools, not one per endpoint

Sixteen tools. Offering every endpoint (~80) makes agents choose badly. Resources and prompts
are not offered. Admin operations (tokens, settings, aspect types, deletes) are not offered.

## Consequences

* The tools live in `internal/api/` (`mcp.go`, `mcp_tools.go`), not a new package: the feed
  publishers and error mappers they must reuse are unexported there, and a package importing
  `internal/api` could not be imported by it. They follow the rule of the package: arguments in,
  a service call, a body out; no SQL, no rules.
* Tickets are addressed by UUID. LaminarFlow tickets have no human key (`LAM-12`) in the data
  model; adding one is a schema change, and it would be a follow-up that the tools adopt.
* A new REST scope rule must be mirrored by hand on its tool. The scope each tool declares is
  next to the tool, and the integration tests pin which scopes see which tools.
* Because the server is stateless, `listChanged` notifications are never delivered; the tool
  set is fixed per build.

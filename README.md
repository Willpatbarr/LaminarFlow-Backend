# LaminarFlow — Backend

The Go API, schema, and migrations for LaminarFlow, a Linear clone with some
added functionality. The React client lives in
[LaminarFlow-Frontend](https://github.com/Willpatbarr/LaminarFlow-Frontend).

Common commands live in [COMMANDS.md](COMMANDS.md). Run the checks with
`./scripts/test.sh` — a bare `go test ./...` skips every database test and
still prints PASS.

Decisions about how the code is structured live in [docs/adr/](docs/adr/) — why something is
the way it is, when the code itself cannot say.

## Changing the API

Endpoints are declared with `huma.Register` in `internal/api/`. The declaration
is the documentation — after changing one, regenerate the spec and commit it:

    go run ./cmd/openapi > api/openapi.json

CI regenerates it and fails if the committed copy differs. The frontend pulls
that file, so a change here is not real until it is committed.

## Using LaminarFlow from Claude Code

The server speaks the [Model Context Protocol](https://modelcontextprotocol.io) at `/mcp`, so a
Claude Code session can find tickets, read them, move them, comment and edit their documents
the way it uses Linear. It authenticates with the same `lam_...` tokens as the API.

1. **Make a token** for the agent (see the API quickstart above), with only the scopes it needs.
   One token per agent, so you can revoke one without touching the rest.
2. **Add the server:**

       claude mcp add --transport http laminarflow https://<host>/mcp --header "Authorization: Bearer lam_xxx"

   Add `--scope project` to write a `.mcp.json` into the repo so every session there gets it
   (keep the token out of git: the header value can read `Bearer ${LAMINARFLOW_TOKEN}`). Run
   `/mcp` inside Claude Code to see the tools. Against a local server the URL is
   `http://localhost:8080/mcp`.
3. **Scopes decide which tools you see.** A tool needs the scope its REST endpoint needs, and a
   token is only offered the tools it may call; calling another anyway is a tool error with
   code `insufficient_scope`. A missing or bad token is a plain 401.

| Tool | Scope | Does |
| --- | --- | --- |
| `search`, `list_tickets`, `get_ticket`, `list_comments`, `get_document`, `list_sprints`, `list_labels` | `ticket:read` | Find and read. Bodies come back as Markdown. |
| `create_ticket`, `update_ticket`, `move_ticket`, `add_comment`, `save_document` | `ticket:write` | Write. Same path as the REST endpoints, so each change is in the activity list and live in open browsers. |
| `list_teams`, `list_statuses`, `list_members` | `team:read` | The ids the other tools take. |
| `list_projects` | `project:read` | |
| `create_team` | `team:write` | Creates a team in your workspace (`name` only), seeded like one made in the app. |
| `create_project` | `project:write` | Creates a project in a team (`team_id`, `name`), with its board. |

Ids are UUIDs. `move_ticket` also takes a status by name (`"In Progress"`). Why it is built this
way: [docs/adr/0008-mcp-endpoint.md](docs/adr/0008-mcp-endpoint.md).

## Where the code lives

| Path | What belongs there |
| --- | --- |
| `main.go` | Startup only: config, pool, schema check, listen. |
| `cmd/` | Operator binaries — `migrate`, `reindex`, `export`. One folder each. |
| `internal/api/` | HTTP handlers, one per endpoint, grouped by resource. Thin. The MCP endpoint and its tools (`mcp*.go`) live here too, for the same reason. |
| `internal/document/` | Rules and SQL for the document write path. |
| `internal/db/` | The Postgres pool. The only package holding a driver. |
| `internal/config/` | Environment settings, parsed and validated once. |
| `internal/migrate/` | The migration runner. `migrations/` holds the SQL. |
| `internal/frontend/` | Serving the embedded bundle and the SPA fallback. |
| `migrations/` | Versioned SQL, one file per change, never edited after merge. |
| `web/` | The embedded frontend bundle. Written by a script, not by hand. |

A handler parses a request and formats a response. Anything that queries
Postgres or enforces a rule belongs in `internal/document/` or a sibling —
`internal/api/` never grows a query.


Deployment lives in [docs/deploy.md](docs/deploy.md) — one image, the Pi and the home
server as two host sections, and an audit showing nothing assumes a particular machine.

The server serves the built React frontend and the API from one origin. Run
`./scripts/build-frontend.sh` to pull the bundle in from the frontend repo; without it
the server still starts and explains what is missing.

## API quickstart

Everything the app does, a script can do over HTTP with a token. The full list of what is
reachable, and with which scope, is [docs/api-parity.md](docs/api-parity.md).

1. **Get a token.** A token is minted by a signed-in person, never by another token. In the app
   it is `/settings/tokens`. Over HTTP, with a session cookie from `POST /api/v1/auth/login`:

       curl -s -b session.jar -X POST localhost:8080/api/v1/auth/tokens \
         -H 'Content-Type: application/json' \
         -d '{"label":"my script","scopes":["ticket:read","ticket:write","project:read"]}'

   The reply's `token` (`lam_...`) is shown once. Scopes are permission names (`ticket:read`,
   `ticket:write`, `aspect:*`, `team:*`, `project:*`, `setting:*`, `account:delete`); notes,
   documents, boards, sprints and the rest of the content use `ticket:*`.
2. **Send it on every call:** `Authorization: Bearer lam_...`.
3. **List, create and update.** Lists are `POST .../list` with `{"limit": 50}` and a `cursor`.
   A create may carry `Idempotency-Key` so a retry is not a duplicate; an update may carry
   `If-Match: "<version>"` (the `ETag` of the last read) so a stale write is a 409
   (`docs/api-conventions.md`, sections 9 and 10).

       POST /api/v1/tickets/list        {"limit": 50}
       POST /api/v1/tickets             {"project_id": "...", "title": "..."}
       PUT  /api/v1/tickets/{id}        {"title": "...", "description": "...", "status_id": "...", "assignee_id": null}
       POST /api/v1/documents/list      {"limit": 50}
       POST /api/v1/documents           {"type": "normal", "title": "...", "project_id": "...", "body": {"content": "..."}}
       PUT  /api/v1/documents/{id}      {"type": "normal", "title": "...", "body": {"content": "..."}}
       POST /api/v1/review-requests     {"document_id": "...", "region_start": 0, "region_end": 5, "body": "...", "reviewer_ids": ["..."]}

4. **Run it all at once:** `LF_TOKEN=lam_... ./scripts/agent-quickstart.sh` (add `LF_URL` for
   another host). It needs `curl` and `jq`, cleans up after itself, and CI runs it against a
   fresh database on every push.

Two things are deliberately session-only: creating, listing and revoking tokens, and
`POST /api/v1/export`. A token calling either gets 403 `insufficient_scope`.

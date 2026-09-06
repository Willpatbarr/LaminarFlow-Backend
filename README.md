# LaminarFlow — Backend

The Go API, schema, and migrations for LaminarFlow, a Linear clone with some
added functionality. The React client lives in
[LaminarFlow-Frontend](https://github.com/Willpatbarr/LaminarFlow-Frontend).

Common commands live in [COMMANDS.md](COMMANDS.md). Run the checks with
`./scripts/test.sh` — a bare `go test ./...` skips every database test and
still prints PASS.

Decisions about how the code is structured live in [docs/adr/](docs/adr/) — why something is
the way it is, when the code itself cannot say.

## Where the code lives

| Path | What belongs there |
| --- | --- |
| `main.go` | Startup only: config, pool, schema check, listen. |
| `cmd/` | Operator binaries — `migrate`, `reindex`. One folder each. |
| `internal/api/` | HTTP handlers, one per endpoint, grouped by resource. Thin. |
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

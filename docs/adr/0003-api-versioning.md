# 3. API versioning — what v1 promises, and what breaks it

Status: Accepted — 2026-09-13 (LAM-54)

## Context

`/api/v1/ping` shipped under LAM-39 with no decision behind the `v1`. The segment was a
literal in one route file, and the OpenAPI document separately declared `0.1.0`. Two numbers,
no stated relationship, and a prefix that every new route would retype.

API Design notes §4 flags versioning and leaves the scheme open. The open part was never
whether to have a version — the path already had one — but what it *promises*, which is the
part a caller depends on and the part that cannot be inferred from the code.

**Who the promise is for.** The web app cannot suffer version skew: `internal/frontend`
serves the bundle from this same binary, so it always talks to the API it was built against.
Versioning exists entirely for the other caller named in API Design notes §5 — scripts and AI
agents holding API tokens, which upgrade on their own schedule or never.

## Decision

**Four things, and they constrain each other.**

### 1. v1 is additive-only

Within `v1` this API adds and never takes away.

| Ships freely in v1 | Requires v2 |
| --- | --- |
| A new response field | Removing or renaming a field |
| A new endpoint | Removing an endpoint |
| A new optional request field | Tightening validation on an existing field |
| | Moving a case to a different status code |

A client is expected to ignore fields it does not recognise. LAM-53 is the worked example: it
added `code` to every error response, which is additive, so it needed no version bump even
though it changed what every error looks like on the wire.

### 2. When v2 ships, v1 keeps answering for a stated window

Both run side by side; `v1` is then removed on an announced date. A sunset that is never
enforced is not a sunset, so the date is part of shipping v2, not a thing to decide later.

### 3. The OpenAPI major tracks the path

`SpecVersion` is `1.0.0`. The major always equals the digit in `V1`, so the two cannot
disagree. The minor moves on each additive change, which is what v1 permits; the patch moves
on a fix that changes no shape.

Before this, the path said `v1` while the document said `0.1.0` — telling a reader "stable
contract" and "pre-1.0, no promises" simultaneously.

### 4. One OpenAPI document per version

`DocsPath`, `OpenAPIPath` and `SchemasPath` move under `V1`. This follows from 2 and 3
together: if v1 and v2 run side by side and the document's major tracks the path, a single
root document cannot be `1.x` and `2.x` at once.

A second reason: inside `/api/` a mistyped docs URL reaches `notFound` and gets the JSON
envelope from ADR-adjacent LAM-53, instead of falling through to the frontend catch-all and
returning the app shell with status 200.

### The prefix is a const, enforced by a test

`V1` is declared once in `internal/api/version.go` and composed into every operation path and
into the three config paths above. `TestEveryRouteCarriesTheVersionPrefix` walks the built
OpenAPI document and fails the build on any operation outside it.

The test is the enforcement, not the const. A const only helps a route that uses it — nothing
stops the next handler writing the prefix out in full, and nothing stops that literal being
`/api/vl/tickets`, which is a 404 indistinguishable from every other 404 on a route nobody has
called yet. Same reasoning as ADR 0001's boundary test: a convention that lives only in a
comment is a convention until someone is in a hurry.

## Consequences

### huma's own prefix mechanism does not do this

`huma.Config` carries `Servers`, and `getAPIPrefix` derives a path prefix from the first
server URL. It reads as though setting it would move everything. It does not:

* `getAPIPrefix` has exactly two callers in v2.39.1 — the docs route and the `$schema` URL
  builder. **`huma.Register` never consults it**, so operation paths are unaffected.
* Setting `Servers` to `/api/v1` moved neither the docs route nor the schema paths when
  tested against v2.39.1. Only setting `DocsPath`, `OpenAPIPath` and `SchemasPath` explicitly
  works.

This was verified rather than assumed, and is recorded here because the config field is
exactly the thing a reader would reach for first.

### The docs routes moved

`/docs`, `/openapi.json` and `/schemas/*` now answer 404 and live under `/api/v1/`. Any
bookmark breaks. Nothing else depends on them — `cmd/openapi` builds the document in-process
rather than fetching it.

### Every `$schema` URL moved with them

`SchemasPath` feeds the link transformer, so the `$schema` field in every response body now
points under `/api/v1/schemas/`. That field is metadata rather than contract — see the known
limitation in `docs/api-errors.md` — so this is not a breaking change under decision 1.

### v2 is a second huma API, not a branch inside this one

One document per version means `NewHumaAPI` grows a sibling rather than conditionals. That is
the intended shape, and it is why `V1` is a const in its own file rather than a parameter.

### Accepted limitation: the guard only covers registered operations

`TestEveryRouteCarriesTheVersionPrefix` reads the OpenAPI document, so it sees huma
operations. A raw `mux.HandleFunc` outside huma is invisible to it — `/healthz` and
`/healthz/db` are exactly that, and are deliberately unversioned, being supervisor probes
rather than API endpoints. A future raw handler under `/api/` would escape the guard. If that
happens, the guard should move to inspecting the mux rather than the document.

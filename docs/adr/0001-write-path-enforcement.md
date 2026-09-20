# 1. Write-path enforcement for the document blob and the search index

Status: Accepted — 2026-09-03 (LAM-5)

## Context

A document's field data lives in two places. `document.body` is a JSON blob and is the source
of truth. `search_index` holds one plain-text row per field and is derived, disposable, and
rebuildable from the blobs at any time.

Two representations of the same data can disagree. The failure is quiet: a write path that
updates the blob but not the index leaves search returning stale results, and nothing crashes.
The architecture notes flagged this when the hybrid model was chosen (§4, Open Follow-Up
Items) and deferred the enforcement design rather than guessing at it.

The question that was left open: do we prevent drift through code structure, or through test
coverage and code review discipline?

## Decision

**Code structure, backed by a mechanical boundary check.** Not discipline alone.

Four mechanisms, weakest to strongest:

1. **`Service.pool` is unexported.** No caller outside this package can reach through a
   `Service` to the database.
2. **`Save` is the only exported write method.** One entry point, one transaction, blob and
   index move together or not at all.
3. **`indexBody` is the only code that inserts `search_index` rows.** It is unexported and
   takes a `pgx.Tx` rather than the pool, so an index write outside the caller's transaction
   is not expressible. `Save` and `RebuildIndex` both route through it.
4. **`boundary_test.go` fails the build** if any package outside `internal/document` issues
   SQL against `document` or `search_index`.

Mechanisms 1–3 make the correct path the easy path. Mechanism 4 exists because they cannot
make the wrong path impossible — see below.

### Why discipline alone was rejected

Tests catch drift in the paths they cover. They say nothing about a path added next quarter by
someone who never read this file. Review catches it only if the reviewer happens to know the
invariant. Both were already true when LAM-3 shipped, and the package comment already asserted
that `Save` was the only write path — an assertion nothing enforced. That is the state this
ADR is correcting, not the state it is preserving.

## Consequences

### What structure alone does not buy

**Go's encapsulation is package-scoped, not project-scoped.** `Service.pool` being unexported
stops a caller reaching through a `Service`. It does not stop a new package calling
`db.Connect`, getting its own `*pgxpool.Pool`, and writing the tables directly. `main.go`
already holds a bare pool today.

There is no language feature that closes this. So `boundary_test.go` parses every `.go` file
in the module and fails on SQL string literals naming either table. It inspects string
literals only, with comments discarded, so prose mentioning a table name is never a failure —
`cmd/reindex/main.go` does exactly that in its package comment and stays green.

**Accepted limitation:** `main.go` keeps a raw pool for the `/healthz/db` readiness check. It
runs `SELECT 1`, touches neither table, and the guard is what keeps that true.

### The guard covers reads, not just writes

Drift is a write problem, but the guard rejects reads too. A read outside this package would
bypass the workspace scoping LAM-4 introduced — `Save` returns `ErrNotFound` for a
cross-workspace document precisely so a caller learns nothing about documents it does not own,
and a raw `SELECT` elsewhere would hand that back. One rule covering both is simpler to state
and simpler to enforce than a rule that tries to distinguish them.

### Sanctioned exception: RebuildIndex

`RebuildIndex` writes `search_index` without writing a blob. That looks like exactly the
violation this ADR forbids, and it is deliberate.

It is a pure function of the blobs: it deletes every index row and regenerates the table from
`document.body`. It cannot introduce drift because it has no input other than the source of
truth. If it ever produces a different index than the live one, the live one was wrong.
`TestRebuildMatchesLiveIndex` asserts the two agree byte for byte — which is also what proves
the live path and the rebuild path have not diverged.

### Naming: Save, not SaveDocument

LAM-5 proposed a single exported `SaveDocument` method. We kept `Save`. Callers write
`document.SaveDocument(...)`, which stutters — against the Effective Go guidance the ticket
itself cited. The package name already carries "document"; the method should not repeat it.

`FieldText` became `fieldText` for the same reason in reverse: it is derivation internals, not
API, and nothing outside the package ever called it.

### Rejected: enforcement in Postgres

Revoking `INSERT`/`UPDATE` on `search_index` from the application role and routing writes
through a `SECURITY DEFINER` function would be a genuinely stronger guarantee — it survives
someone bypassing the Go layer entirely.

It was rejected as disproportionate. It splits the invariant across two languages, complicates
every migration and the throwaway-database test bootstrap, and defends against a threat model
(a second application writing this database) that the architecture explicitly rules out: the
Go backend is the only component that talks to Postgres. Revisit this if that ever stops being
true.

### Gotcha: a lint error hides the guard's result

`scripts/test.sh` runs under `set -e` with gofmt, build, vet, and staticcheck all ahead of
`go test`. A file that trips staticcheck — an unused constant, say — exits the script before
the boundary test ever runs, and the output looks like the test was skipped rather than
blocked.

This bit twice while verifying the guard was real. If the boundary test appears not to run,
read the gate above it before suspecting the test.

## Extended to api_token — LAM-55

The same four mechanisms now guard `api_token` from inside `internal/auth`, and for a
different reason than drift.

API Design notes §5 chose a direct Postgres lookup for token validation, and that was only
safe to choose because a cache can be added later *behind a single entry point* — "purely an
internal change to that one function [requiring] no changes to any endpoint that uses it."
An endpoint that queries `api_token` itself removes that option permanently, and nothing
about the endpoint would look wrong while doing it. So the guard here protects a future
decision rather than a present invariant.

**The walk moved to `internal/sqlguard`.** It was a 60-line AST walk inside
`internal/document`; a second owner meant either copying it or sharing it. The subtle parts —
parsing with comments discarded, skipping the owner's own directory, finding the module root
rather than assuming a depth — are all invisible when they are wrong, which is the argument
`internal/dbtest` already makes for itself.

**One exemption, and it is narrow.** `internal/migrate` is skipped by every guard. A schema
constraint test proves a constraint fires by violating it, so naming the table in an `INSERT`
is the whole technique — there is no way to assert "`api_token` rejects a null account"
without SQL naming `api_token`. E-LAM-0003 parked those tests there for every table with no
owning package. Tables that *do* have an owner keep their schema tests with the owner, which
is why `internal/document` holds `schema_document_test.go` and `schema_search_index_test.go`
and needs no exemption at all.

That exemption should shrink. `api_token` has an owner now, so
`internal/migrate/schema_api_token_test.go` belongs in `internal/auth`; moving it requires
porting `migratedPool` and `wantPgError`, which is its own piece of work.

## search_index moved out — LAM-45

`internal/document` no longer owns `search_index`. That ownership was right while the index
was one document's derived data; LAM-45 added tickets and comments as sources, and an index
derived from three tables is nobody's derived data in particular. It is `internal/search`'s
now, and `Save` calls into it inside its own transaction — so the blob and its rows still
move together or not at all. The ownership moved; the invariant did not.

### The guard gained a read/write split, and that is the interesting part

Guarding reads as well as writes was correct for `document`: a raw `SELECT` elsewhere would
bypass the workspace scoping `Save` enforces, and one rule covering both is simpler to state.
That reasoning does not survive contact with an index package. `internal/search` exists to
denormalise `workspace_id`, `project_id`, `team_id` and a title off `document`, `ticket` and
`comment` onto the row describing each one. Reading those tables *is* the work.

So `sqlguard.Rule` now separates the two:

* **Writes** — `insert into`, `update`, `delete from` — are owner-only and **not grantable**.
  Drift is a write problem, and this half has no exceptions.
* **Reads** — `from`, `join` — may be granted to named packages, with the reason recorded at
  the call site.

Two grants exist. `internal/search` reads `document` because scope denormalisation requires
it. `internal/document` reads `search_index` because its tests must observe the rows `Save`
caused — `aspect_seam_test.go` proves a field uuid survives from body key to index row
unchanged, which cannot be asserted without reading the index.

The split makes the guard stricter where it matters: before, granting any access meant
granting both.

### Schema tests follow ownership

`schema_search_index_test.go` moved to `internal/search` with the table, matching the rule
`internal/document` already followed for its own. Its fixture now creates documents through
`document.Service.Save` rather than a raw `INSERT` — which the guard caught, correctly, the
moment the file changed packages.

## Verification

The guard was confirmed to fail, not just to pass. A throwaway file containing
`SELECT * FROM search_index` was added outside `internal/document` — once under `internal/db`,
once at the repo root — and the test failed in both cases naming the exact file and position,
then passed once the file was removed. A guard that has never failed proves nothing.

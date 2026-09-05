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

## Verification

The guard was confirmed to fail, not just to pass. A throwaway file containing
`SELECT * FROM search_index` was added outside `internal/document` — once under `internal/db`,
once at the repo root — and the test failed in both cases naming the exact file and position,
then passed once the file was removed. A guard that has never failed proves nothing.

# Migrations

Numbered SQL files, applied in version order by the runner in
`internal/migrate`. They are embedded in the binary (`embed.go`), so the
`migrate` command and the server both carry their own schema history — there is
no migrations directory that has to travel beside the binary.

    go run ./cmd/migrate up       # apply everything pending
    go run ./cmd/migrate down     # roll back the most recent
    go run ./cmd/migrate status   # what has run, what has not

See COMMANDS.md for the forms that load `.env`.

## File format

One file per change, named `NNNN_name.sql`, holding both halves:

    -- A comment block explaining why this change exists.
    -- Anything above the Up marker is ignored by the runner.

    -- +migrate Up

    CREATE TABLE thing (...);

    -- +migrate Down

    DROP TABLE thing;

Both markers are required. Up and Down live in one file on purpose: they are two
halves of one reversible change, and splitting them across two files is an
invitation to update one and not the other.

A Down section may be left empty when a change genuinely cannot be reversed.
That is a deliberate declaration, not a shortcut — `migrate down` refuses with
`migration declares no Down section` rather than reporting success having
changed nothing.

## Guarantees

Each migration runs in one transaction together with its own `schema_migrations`
row. Postgres has transactional DDL, so a migration that fails halfway leaves
neither the schema change nor the bookkeeping — the two cannot disagree about
what has run.

The runner takes a Postgres advisory lock first, so two servers starting at once
cannot both decide the same migration is pending.

The test suite applies every migration to an empty database on each run using
this same runner (see `internal/document/main_test.go`), so the fresh-install
path a self-hosted user takes is exercised continuously. `internal/migrate`
additionally applies the real migrations *and reverses them* on every run, so a
Down section that does not actually work fails the build.

## Adopting a hand-built database

A database whose schema was applied by hand before this runner existed — which
is where LAM-2 through LAM-5 left the development database — has the tables but
no `schema_migrations`. Running `up` there would try to create `document` a
second time and fail.

    go run ./cmd/migrate baseline

records every known migration as applied without running any of them. It refuses
on a database that already has recorded migrations, so it cannot be used to
paper over a genuinely failed run.

## Expand and contract

Never let one migration both tighten a constraint and require application code
that does not exist yet. Split it across two migrations with a deploy between:

1. **Expand** — add the column nullable.
2. **Backfill** — populate the existing rows.
3. **Ship the code** that writes the column on every new row.
4. **Contract** — a later migration sets `NOT NULL`.

Old code keeps working at every step, so the tree is never broken between the
schema change and the code change that depends on it.

LAM-4 did not do this. `0003_workspace.sql` added `document.workspace_id` as
`NOT NULL` in a single step, which broke every INSERT in `Save` the moment it
was applied. Nothing caught it: `go build` does not compile test files, and the
database tests were silently skipping.

### Do not paper over it with a DEFAULT

The tempting shortcut is to give the new column a `DEFAULT` so existing inserts
keep working. For `workspace_id` that would have been actively harmful — a
default workspace ID is precisely the "there is only ever one workspace"
assumption LAM-4 existed to remove. It would have hidden the breakage by baking
in the bug.

A `DEFAULT` is right when the value is genuinely a property of the column, like
`created_at DEFAULT now()`. It is wrong when it invents an answer to a question
the caller is supposed to answer.

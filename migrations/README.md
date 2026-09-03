# Migrations

Numbered SQL files, applied in filename order. There is no migration runner
yet — LAM-10 owns that — so against a real database they are applied by hand:

    set -a && source .env && set +a && psql "$DATABASE_URL" -1 -f migrations/0003_workspace.sql

The test suite applies every file to an empty database on each run (see
`internal/document/main_test.go`), so the fresh-install path a self-hosted user
takes is exercised continuously. A migration that only works against an
already-populated database will fail there.

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

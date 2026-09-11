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

## Foreign keys are not indexed for you

Postgres builds an index for a `PRIMARY KEY` and for a `UNIQUE` constraint. It
does not build one for a foreign key column. A child table whose only index is
its own primary key will scan the whole table every time it is looked up by
parent.

That does not mean every foreign key needs its own index. A `UNIQUE` constraint
on `(parent_id, name)` — which most child tables in this schema want anyway —
builds a `btree (parent_id, name)`, and a btree index serves lookups on its
leading column. So the constraint already indexes the foreign key, and adding a
separate single-column index would be redundant.

`0005_team.sql` is the worked case: `UNIQUE (workspace_id, name)` is the only
index on `workspace_id`, and the file says so where a reader would otherwise
assume the index was forgotten.

The order matters, and it is the part that breaks silently. Reverse the
constraint to `(name, parent_id)` and the foreign key loses its index with no
error and no failing test — unless something asserts the leading column.
`TestTeamConstraints/workspace_id_leads_an_index` in `internal/migrate` is that
assertion, and a new child table should carry the same one.

### Write the decision down either way

Whether a table gets an index or not, say so in the migration and say why. An
absent index with no comment reads as an oversight, and the next person either
adds a redundant one or spends an afternoon working out whether the omission was
deliberate. `0003_workspace.sql` declines an index and names the ticket that
should add it; that is the pattern.

### A cascading foreign key needs the index even when no query does

`ON DELETE CASCADE` and `ON DELETE SET NULL` are not free at the parent end.
Before Postgres can delete a parent row it has to find every child row that
references it, and it uses the child's index to do that. Without one, deleting
a single row scans the whole child table.

That makes the index load-bearing even when no application query would use it.
`ticket.status_id` and `ticket.assignee_account_id` in `0011_ticket.sql` are
both indexed for exactly this reason — nothing reads tickets by status alone or
by assignee across projects, but deleting one status or one account would
otherwise scan every ticket in the instance.

So the question in the section above splits in two:

* Does a **query** need this index? Maybe not.
* Does an **`ON DELETE` action** need it? If the action is `CASCADE` or
  `SET NULL`, yes, always.

A `RESTRICT` foreign key is the exception. It still has to find a referencing
row, but it can stop at the first one, so the cost is bounded.

## A join table between two children of the same parent

`ticket_sprint` joins `ticket` and `sprint`. Both are children of `project`, and
two plain foreign keys let the join table pair a ticket in one project with a
sprint in another. Nothing in the database objects, and the symptom surfaces
much later as a board quietly showing a ticket from somewhere else.

The fix is to carry the shared parent's key on the join table and reach both
sides through composite foreign keys:

    ALTER TABLE ticket ADD CONSTRAINT ticket_id_project_key UNIQUE (id, project_id);
    ALTER TABLE sprint ADD CONSTRAINT sprint_id_project_key UNIQUE (id, project_id);

    CREATE TABLE ticket_sprint (
        ticket_id  uuid NOT NULL,
        sprint_id  uuid NOT NULL,
        project_id uuid NOT NULL,

        PRIMARY KEY (ticket_id, sprint_id),

        FOREIGN KEY (ticket_id, project_id) REFERENCES ticket (id, project_id) ON DELETE CASCADE,
        FOREIGN KEY (sprint_id, project_id) REFERENCES sprint (id, project_id) ON DELETE CASCADE
    );

One `project_id`, two references, so both parents are in the same project by
construction. There is no state in which they disagree and no application code
that has to remember to check.

Three things about it are worth knowing before reaching for it:

* **The parents pay.** Postgres only points a foreign key at columns carrying a
  unique constraint, so each parent needs `UNIQUE (id, project_id)` purely as a
  target. Both are logically redundant — `id` is already the primary key — and
  each builds a second btree on a table that did not ask for one.
* **`project_id` needs no foreign key of its own.** It can only hold a value
  that both parents already carry, and both of them reference `project`.
* **It blocks reparenting.** With the default `ON UPDATE NO ACTION`, moving a
  ticket to another project while it sits in one of the old project's sprints
  is rejected. `ON UPDATE CASCADE` does not help — it drags the join row's
  `project_id` along and the sprint side fails instead. Whatever moves a row
  between parents has to unlink it first. Assert that in a test, or the next
  person meets it in production.

### It is not always right

The same pattern was proposed for `team_member` under LAM-42 and rejected. That
turned on outside collaborators being real: a person on a team without being a
member of the workspace above it is a case the product wants, so forcing the two
memberships to agree would have banned a feature.

So the question is not "do both sides share a parent" but "is a mismatch ever
legitimate". For `ticket_sprint` it never is. For `team_member` it routinely is.
Answer that first; the DDL follows.

## Enforcing "the parent is in a particular state"

A child table sometimes only makes sense under a parent in some state.
`comment_reviewer` is the worked case: a reviewer can only be attached to a
`comment` that is acting as a review request, meaning its `review_status` is
non-null. LAM-25 assigned that rule to application code. The database can hold
it, and the technique generalises.

The naive version — a composite foreign key on the state column itself —
does not work when the state is mutable:

    FOREIGN KEY (comment_id, review_status) REFERENCES comment (id, review_status)

`review_status` moves from `pending` to `approved`, which is the entire point
of a review. That update changes the referenced key, so it either cascades on
every transition or is refused outright. Both are wrong.

Reference a **generated column that summarises the state** instead, chosen so
it only changes at the boundary that actually matters:

    ALTER TABLE comment
        ADD COLUMN is_review_request boolean
            GENERATED ALWAYS AS (review_status IS NOT NULL) STORED;

    ALTER TABLE comment ADD CONSTRAINT comment_id_review_key UNIQUE (id, is_review_request);

    CREATE TABLE comment_reviewer (
        comment_id        uuid    NOT NULL,
        account_id        uuid    NOT NULL REFERENCES account(id) ON DELETE CASCADE,
        is_review_request boolean NOT NULL DEFAULT true,

        PRIMARY KEY (comment_id, account_id),

        CHECK (is_review_request),

        FOREIGN KEY (comment_id, is_review_request)
            REFERENCES comment (id, is_review_request) ON DELETE CASCADE
    );

`pending → approved` leaves the generated boolean at `true`, so the foreign
key never notices. Only crossing the null boundary moves it.

Four things about it are worth knowing before reaching for it:

* **Both halves are load-bearing.** The foreign key stops a row pointing at a
  parent in the wrong state. The `CHECK` stops a caller writing `false`
  explicitly, which would otherwise match a plain comment's `(id, false)` and
  pass the foreign key cleanly. Dropping either one opens a different hole,
  and `TestCommentReviewerConstraints` has a test for each.
* **Leave `ON UPDATE` alone.** The only change the referenced key can undergo
  is the one that must be refused, so a cascade would never usefully fire, and
  declaring one claims the child follows the parent when it never can.
  `NO ACTION` says what actually happens.
* **It blocks the reverse transition.** A review request with reviewers
  attached cannot become a plain comment; the reviewers have to be unassigned
  first. That is usually correct — the alternative is silently dropping them —
  but it is a real sequencing rule and belongs in a test.
* **The parent pays for it.** A stored boolean on every parent row, plus a
  second btree for the `UNIQUE` the foreign key needs as a target. The same
  cost `0013` pays on `ticket` and `sprint`.

### When not to

If the parent state is genuinely allowed to change underneath existing
children, this is the wrong tool — it will block exactly that. Check that the
boundary you generate on is one the product agrees is one-way while children
exist.

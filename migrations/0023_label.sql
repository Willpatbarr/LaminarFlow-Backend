-- LAM-49: the label table.
--
-- Master spec 3.1 makes labels load-bearing rather than decorative:
--
--   "Bug, feature, task, etc. should not require separate core entities. They
--    can be represented using configurable labels or fields."
--
-- So this is the only mechanism the product has for saying what kind of work
-- a ticket is. There is no ticket.kind and no issue-type table, deliberately.
-- Whatever renders a bug badge reads it from here.
--
-- Child of team, matching status and aspect_type. Workflows and vocabulary
-- are both configured per team, so two teams in one workspace can label their
-- work entirely differently. CASCADE: a label without its team is nothing.
--
--
-- UNIQUE (team_id, name), and why this table breaks the pattern
--
-- project, status and aspect_type all decline this constraint, and the epic's
-- standing rule is not to invent constraints a ticket does not ask for. This
-- one is asked for, and the asymmetry is real rather than a lapse.
--
-- A duplicate status name is a cosmetic annoyance: statuses are picked from a
-- small ordered list, a board column shows one at a time, and the position
-- ordering keeps two "In Review" rows visually distinct. A duplicate label
-- name is a silent data bug. Labels are chips - the user sees "bug" twice
-- with no way to tell the ids apart, tags half their tickets with one and
-- half with the other, and then filtering by "bug" returns half the results
-- with nothing anywhere reporting an error. The failure is invisible at
-- exactly the moment it matters.
--
-- The constraint is also the index. A btree on (team_id, name) serves the
-- foreign key, its CASCADE, and the label picker's read - one team's labels
-- in name order - so LAM-49 step 1's requested index is this constraint and
-- no separate index is built. team_id leads, which is what makes that true;
-- reversed to (name, team_id) the foreign key silently loses its index. See
-- "Foreign keys are not indexed for you" in migrations/README.md, and the
-- assertion in the tests.
--
--
-- No CHECK on color, matching 0010_status.sql. Whether a color is hex, a
-- palette token or something else is the application's to define and no
-- ticket has settled it. Constrain it in the ticket that settles the palette
-- rather than guessing twice.
--
-- No position column. status has one because a board renders statuses in a
-- fixed order; labels are a set, not a sequence, and nothing has asked for
-- one. Adding it later is an ALTER TABLE.
--
-- Nothing seeds default labels, so a team with none is legal and every label
-- picker must render empty. That is the same homeless bootstrap obligation
-- LAM-43 owns for statuses and aspect types.
--
-- Labels attach to tickets only. 0024_ticket_label.sql says why, and why a
-- polymorphic target_type is not the answer if documents ever want them.

-- +migrate Up

CREATE TABLE label (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    uuid        NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    name       text        NOT NULL,
    color      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Both the uniqueness claim and the index. team_id leads deliberately, so
    -- the foreign key, the CASCADE and the picker read all come off this one
    -- btree and no separate index on team_id is built.
    CONSTRAINT label_team_name_key UNIQUE (team_id, name)
);

-- No CHECK on color. The format is the application's to define and LAM-49
-- does not say which - the same call 0010_status.sql made.

-- +migrate Down

DROP TABLE label;

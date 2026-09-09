-- LAM-19: the sprint table.
--
-- A timebox, not a structural parent. A sprint associates tickets from
-- anywhere in its project rather than owning them, so nothing hangs off it -
-- the association lives in ticket_sprint (LAM-20). CASCADE on project_id all
-- the same: a timebox for a project that no longer exists is not a timebox.
--
-- start_date and end_date are both nullable, so a sprint can be created and
-- named before its dates are settled. Everything reading dates carries that
-- case.
--
-- end_date is stored, not derived. The intended flow is that a team's sprint
-- length is a setting and a new sprint's end_date is computed from it at
-- creation - but computed once, and written down. Deriving it on read from a
-- mutable setting would mean changing the team's cadence retroactively moves
-- the end date of every sprint that already ran. It is also the field someone
-- edits when one sprint gets extended by three days, which a derived value
-- could not express.
--
-- The sprint length itself is deliberately not a column here, and not a
-- column on project either. It is a team-scoped row in setting: the same
-- class of thing as the default statuses and Kanban columns the schema notes
-- name as team-level settings, and team-scoped for the same reason status is
-- - a cadence is a team's rhythm. setting already carries team scope, so this
-- costs no migration. A project-level override would need setting's third
-- scope, which is a migration to pay for when a project actually needs to
-- differ from its team.
--
-- Overlapping sprints are allowed. Postgres could forbid them with an EXCLUDE
-- over daterange per project, and that is deliberately not done: a team may
-- run parallel tracks, or leave a sprint open past the start of the next one,
-- and a constraint here would be a product rule nobody asked for.
--
-- No UNIQUE (project_id, name) either, matching project and status. LAM-19
-- does not ask for one.

-- +migrate Up

CREATE TABLE sprint (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid        NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    name       text        NOT NULL,
    start_date date,
    end_date   date,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- A sprint that ends before it starts is not a product decision, it is
    -- incoherent. Nullable dates need no special handling: SQL evaluates this
    -- to NULL when either side is missing, and a CHECK only rejects on false.
    CONSTRAINT sprint_ends_after_it_starts CHECK (end_date >= start_date)
);

-- The read this table gets: one project's sprints in chronological order.
-- project_id leads, so this also serves the foreign key and its CASCADE.
CREATE INDEX sprint_project_start_idx ON sprint (project_id, start_date);

COMMENT ON COLUMN sprint.end_date IS
    'Stored, not derived. Computed from the team sprint-length setting at creation, then owned by the sprint.';

-- +migrate Down

DROP TABLE sprint;

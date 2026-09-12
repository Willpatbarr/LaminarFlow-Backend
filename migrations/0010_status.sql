-- LAM-17: the status table.
--
-- Child of team - workflows are configured per team, so two teams in one
-- workspace can run entirely different boards. CASCADE: a status belongs to
-- its team and means nothing without it.
--
-- No status is sacred (master spec 3.4). name, color and position are all
-- user-owned: renameable, recolourable, reorderable, deletable. category is
-- not. It exists so reporting - burndown charts, "open tickets" filters - can
-- work generically over statuses whose names it cannot know, which only holds
-- if the set of categories is closed. Hence the CHECK: a typo like
-- 'inprogress' would otherwise drop a whole status out of every chart with no
-- error anywhere.
--
-- position is deliberately NOT unique per team. Reordering is then a plain
-- UPDATE; a UNIQUE (team_id, position) would collide halfway through a swap
-- unless it were deferrable or renumbered through a temporary value. Two
-- statuses may share a position, and readers sort by (position, name) so the
-- result is still deterministic.
--
-- No UNIQUE (team_id, name) either, because LAM-17 does not ask for one. Two
-- statuses in a team may share a name. Stated here so the absence does not
-- read as an oversight, and asserted in the tests so adding uniqueness later
-- has to be deliberate.
--
-- Deleting a status that tickets still use: decided as SET NULL, so those
-- tickets become statusless rather than blocking the delete. That FK lives on
-- ticket.status_id, not here, so LAM-18 implements it - and it means
-- ticket.status_id must be nullable, which LAM-18's field list does not
-- currently say.

-- +migrate Up

CREATE TABLE status (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    uuid        NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    name       text        NOT NULL,
    color      text        NOT NULL,
    position   integer     NOT NULL,
    category   text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Closed set, unlike name. Reporting depends on these three values
    -- meaning the same thing in every team on every instance.
    CONSTRAINT status_category_is_known
        CHECK (category IN ('not_started', 'in_progress', 'done'))
);

-- The index LAM-17 step 2 asks for on team_id, widened to carry position.
-- team_id leads it, so the foreign key lookup is served exactly as a
-- single-column index would serve it, and the read this table actually gets -
-- one team's statuses in board order - comes back sorted for free. The name
-- tiebreak is left to the sort, since it only ever breaks ties among a
-- handful of rows.
CREATE INDEX status_team_position_idx ON status (team_id, position);

-- No CHECK on color. The format - hex, a named palette token, something else
-- - is the application's to define, and LAM-17 does not say which. Constrain
-- it in the ticket that settles the palette rather than guessing here.

COMMENT ON COLUMN status.category IS
    'Closed set: not_started, in_progress, done. Reporting groups by this because status names are user-defined.';

-- +migrate Down

DROP TABLE status;

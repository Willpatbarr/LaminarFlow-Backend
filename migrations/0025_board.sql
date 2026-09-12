-- LAM-48: the board and board_column tables.
--
-- A board is its own editable entity, the way it is in Jira: you create one,
-- name it, configure its columns and change its settings. Master spec 3.5
-- makes Kanban the primary work-tracking view but never models it as a thing
-- you can own, so this is the first migration to say what a board IS.
--
-- Many boards per project, and nothing here privileges one of them. Two
-- people wanting different column arrangements of the same project is the
-- normal case rather than a conflict, so there is no UNIQUE (project_id,
-- name) and nothing marking a default. Asserted in the tests, so adding
-- uniqueness later has to argue with something.
--
--
-- Board and saved view are two tables, not one with a kind
--
-- Settled across LAM-48 and LAM-50 before either was written, because the
-- answer changes both. A board owns column configuration; a saved view owns
-- filters, sorting and visible fields. One table with a kind column would
-- leave half its columns null on every row - which is the shape LAM-42
-- rejected polymorphism for, and the shape LAM-23, LAM-24 and LAM-26 rejected
-- before it.
--
-- The consequence lands on LAM-50: a saved view with layout = 'board' has to
-- name WHICH board, so saved_view gets a nullable board_id and a biconditional
-- CHECK tying it to the layout, following document_aspect_type_matches_type
-- in 0016. That is LAM-50's migration to write; it is recorded here because
-- this file is where the split was decided.
--
--
-- Why board_column_status is a third table (0026)
--
-- The easy mistake is a status_id column on board_column. In Jira a single
-- column routinely holds several statuses - a "Done" column mapping Done,
-- Won't Do and Duplicate at once - and one column per status would quietly
-- rule that out with no way back except a migration. The mapping is
-- many-to-many, so it is its own table.
--
-- That is also why a column has its own name. A column's label is not a
-- status name; a column called "In Review" may hold three statuses with
-- other names. Naming the column separately is the entire point of a board.
--
--
-- group_by, and the second value that is coming
--
-- Master spec 3.5: "Columns should eventually be able to map to configurable
-- fields, not only status." So columns derive from a source, and status is
-- only the first one. group_by is text with a CHECK holding exactly 'status'
-- today, the same closed-set shape status.category uses and for the same
-- reason: every renderer branches on it, so a typo like 'statuses' must fail
-- at write time rather than silently dropping a board out of every view.
--
-- LAM-49 landed label and ticket_label two migrations ago and label is the
-- obvious second value. It is deliberately NOT in the CHECK yet.
-- board_column_status is status-specific by design, so widening the CHECK
-- without a matching board_column_label table would let someone create a
-- board that cannot render its own columns. Widening it later is an ALTER
-- TABLE and a wider CHECK - a one-line migration, not a redesign - and that
-- migration is the one that should also build the second mapping table.
--
--
-- The cross-team status hole, and where the price is paid
--
-- board -> project -> team, and status -> team, and nothing so far makes them
-- agree. A board could name its columns after another team's statuses, and
-- the symptom is a board rendering columns that no ticket in the project can
-- ever be in.
--
-- Closing it needs team_id somewhere on the board chain, and this file puts
-- it on all three tables rather than on the leaf. Every table carries team_id
-- and reaches its parent through the pair, so the team is fixed at the top
-- and carried down by construction:
--
--   board.(project_id, team_id)             -> project (id, team_id)
--   board_column.(board_id, team_id)        -> board (id, team_id)
--   board_column_status.(status_id, team_id) -> status (id, team_id)      [0026]
--   board_column_status.(board_column_id, team_id) -> board_column (id, team_id)
--
-- The alternative was to denormalise nothing until the leaf and walk the
-- whole chain from board_column_status, which would have meant three
-- denormalised columns on one join table and a four-hop reference. Carrying
-- one column down three tables is cheaper to read and cheaper to enforce.
--
-- This is the opposite call to the one 0024_ticket_label.sql made, and the
-- difference is worth naming so the two files do not read as inconsistent.
-- There, denormalising team_id would have meant adding a column to ticket -
-- an existing table with many writers, whose own ticket deliberately left it
-- out, and to which no ticket-level read wants it. Here all three tables are
-- new in this migration and the next, and a board is genuinely team-bound
-- rather than merely project-bound: its columns are built out of team
-- statuses, so the team is part of what a board IS. The column is paid for by
-- the table that needs it, in both cases.
--
-- project already carries UNIQUE (id, team_id) from 0024, so the top of the
-- chain costs nothing new. board and board_column declare their own targets
-- inline, since both tables are created here. status pays in 0026.
--
-- The consequence is the one 0024 already introduced: a project whose boards
-- exist cannot be moved to another team without deleting them first. Correct
-- for the same reason - a project changing teams would leave its boards
-- rendering columns made of the old team's statuses.
--
--
-- position is NOT unique per board, matching status.position and
-- aspect_type_field.position and for the reason 0010 gives: reordering stays
-- a plain UPDATE, where a UNIQUE (board_id, position) would collide halfway
-- through a swap unless it were deferrable or renumbered through a temporary
-- value. Two columns may share a position and readers sort by
-- (position, name) so the result is still deterministic. Asserted with a
-- two-update swap, as TestAspectTypeFieldConstraints does.
--
-- is_visible is NOT NULL DEFAULT true. Master spec 3.5 calls column
-- visibility a per-view setting; this is the board's own default arrangement,
-- and a view that hides more is LAM-50's config to hold. Not nullable, so
-- nothing has to decide what a null visibility means and the flexible-
-- hierarchy guard has nothing new to pin.
--
-- Nothing seeds a default board. A project with none is legal and must
-- render, which is the same homeless bootstrap obligation LAM-43 owns for
-- statuses and aspect types, and where this one belongs too.
--
-- One thing this migration deliberately does not fix: the Kanban-columns
-- setting the Database Schema notes 2 describe is a DIFFERENT thing from a
-- board - it is the menu of sources a board may derive columns from, and a
-- board is one saved arrangement built from that menu. No such setting key
-- exists yet, and setting.key is unconstrained text with no registry, so a
-- typo is a silently ignored setting. Defining that key somewhere real is a
-- job for whichever ticket introduces it; inventing it here would be
-- inventing structure nobody has asked for.

-- +migrate Up

CREATE TABLE board (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    name       text        NOT NULL,
    group_by   text        NOT NULL DEFAULT 'status',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- The top of the chain. project already carries UNIQUE (id, team_id)
    -- from 0024, so this reference costs the parent nothing new.
    CONSTRAINT board_project_fkey FOREIGN KEY (project_id, team_id)
        REFERENCES project (id, team_id) ON DELETE CASCADE,

    -- Closed set, holding one value today. See the why block for what it
    -- takes to add the second.
    CONSTRAINT board_group_by_is_known CHECK (group_by IN ('status')),

    -- Target for board_column below. Redundant as a uniqueness claim - id is
    -- already the primary key - and there only so the team can be carried
    -- down the chain.
    CONSTRAINT board_id_team_key UNIQUE (id, team_id)
);

-- One project's boards, in name order. project_id leads, so this also serves
-- the CASCADE that reaches board when a project is deleted. No uniqueness:
-- two boards of one project may share a name.
CREATE INDEX board_project_name_idx ON board (project_id, name);

CREATE TABLE board_column (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    board_id   uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    name       text        NOT NULL,
    position   integer     NOT NULL,
    is_visible boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- The middle of the chain. A column without its board is nothing, and it
    -- inherits the board's team rather than naming its own.
    CONSTRAINT board_column_board_fkey FOREIGN KEY (board_id, team_id)
        REFERENCES board (id, team_id) ON DELETE CASCADE,

    -- Target for board_column_status in 0026, for the same reason as above.
    CONSTRAINT board_column_id_team_key UNIQUE (id, team_id)
);

-- The read a board does on every load: its columns in board order. board_id
-- leads, so this serves the CASCADE too. The name tiebreak is left to the
-- sort, since it only ever breaks ties among a handful of rows - the same
-- call 0010 made for status_team_position_idx.
CREATE INDEX board_column_board_position_idx ON board_column (board_id, position);

COMMENT ON COLUMN board.team_id IS
    'Denormalised from project. Carries no reference of its own; the composite foreign key forces it to equal the project''s team, and board_column and board_column_status inherit it from here.';
COMMENT ON COLUMN board_column.name IS
    'The column''s own label, deliberately not a status name. One column may map several statuses; see board_column_status.';

-- +migrate Down

DROP TABLE board_column;
DROP TABLE board;

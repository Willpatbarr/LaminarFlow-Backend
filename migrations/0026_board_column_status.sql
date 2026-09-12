-- LAM-48: board_column_status, the mapping from a board column to the
-- statuses it holds.
--
-- The table the whole three-table shape exists for. A status_id column on
-- board_column would force one status per column, and in practice a "Done"
-- column maps Done, Won't Do and Duplicate at once. The relationship is
-- many-to-many - a column holds several statuses, and nothing stops one
-- status appearing in two columns of two different boards - so it is its own
-- table with a composite primary key on (board_column_id, status_id). No
-- surrogate id; the pair is the fact.
--
-- It is deliberately status-SPECIFIC. 0025 gives board a group_by holding
-- only 'status' today, and master spec 3.5 wants columns to derive from
-- configurable fields eventually. When a second source lands - label is the
-- obvious one now that LAM-49 has shipped it - it gets its own mapping table
-- or a generalised one, decided then. Generalising now, with exactly one
-- source in existence, would be inventing structure nobody has asked for.
--
--
-- The last hop of the team chain
--
-- 0025 put team_id on board and board_column and carried it down by
-- composite foreign key. This table closes the loop: its team_id must equal
-- both the column's team and the status's team at once, so a board cannot
-- render columns made of another team's statuses.
--
--   (board_column_id, team_id) -> board_column (id, team_id)
--   (status_id,       team_id) -> status       (id, team_id)
--
-- board_column already carries UNIQUE (id, team_id) from 0025. status does
-- not, so it pays here - the same price ticket and sprint paid in 0013 and
-- project and label paid in 0024. Logically redundant, since id is already
-- unique on status, and it exists only as a reference target.
--
-- team_id carries no foreign key of its own to team. It cannot hold a value
-- that both references have not already established.
--
--
-- Deletes
--
-- CASCADE on both. Master spec 3.4 is explicit that no status is sacred, and
-- LAM-17 built status to be deletable for that reason - so deleting a status
-- must remove it from every board column rather than blocking the delete. The
-- column itself survives, possibly holding no statuses at all, which is a
-- legitimate state: an empty column renders empty rather than disappearing.
-- Both halves asserted, because "deleting a status deletes the column" is the
-- destructive misreading this table is one keystroke away from.
--
-- Deleting a board column removes its mappings, which are not a record of
-- anything without it. Deleting the board reaches here through board_column,
-- and deleting the project or team reaches it through board.
--
-- ON UPDATE is NO ACTION, so a board cannot be moved to a project in another
-- team while its columns map statuses, and a status cannot be moved to
-- another team while any board column holds it. Both fall out of the chain
-- and both are correct: either move would leave a board holding statuses its
-- team does not own.
--
--
-- Two indexes, both directions
--
-- The primary key is a btree led by board_column_id, so "which statuses does
-- this column hold" - the read every board load does, once per column - and
-- the column-side CASCADE are both served already. A separate index on
-- board_column_id would be a second copy of it.
--
-- The other direction has nothing: "which columns hold this status" is what
-- the status-side CASCADE walks when a status is deleted, and what a board
-- renderer asks in reverse when placing a ticket. Built as
-- (status_id, board_column_id) so both come off the index alone.

-- +migrate Up

-- Foreign key target for the status reference below. Redundant as a
-- uniqueness claim and present only because Postgres requires a unique
-- constraint on referenced columns. board_column already carries its own
-- from 0025.
ALTER TABLE status ADD CONSTRAINT status_id_team_key UNIQUE (id, team_id);

CREATE TABLE board_column_status (
    board_column_id uuid        NOT NULL,
    status_id       uuid        NOT NULL,
    team_id         uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (board_column_id, status_id),

    -- One team_id, reached through both parents, so a column cannot hold a
    -- status belonging to a different team.
    CONSTRAINT board_column_status_column_fkey FOREIGN KEY (board_column_id, team_id)
        REFERENCES board_column (id, team_id) ON DELETE CASCADE,
    CONSTRAINT board_column_status_status_fkey FOREIGN KEY (status_id, team_id)
        REFERENCES status (id, team_id) ON DELETE CASCADE
);

-- Which columns hold one status: the status-side CASCADE's index, and the
-- reverse lookup a renderer does when placing a ticket. board_column_id
-- follows so both come out of the index without a heap touch. The forward
-- direction needs nothing - the primary key is already a btree led by
-- board_column_id.
CREATE INDEX board_column_status_status_idx
    ON board_column_status (status_id, board_column_id);

COMMENT ON COLUMN board_column_status.team_id IS
    'Denormalised from both parents. Carries no reference of its own; the two composite foreign keys force it to equal the column''s team and the status''s team at once.';

-- +migrate Down

DROP TABLE board_column_status;

ALTER TABLE status DROP CONSTRAINT status_id_team_key;

-- LAM-85: ticket.position, the order of cards within a board column.
--
-- A Kanban column is the tickets of one status, and a person drags a card to
-- where it belongs. Nothing in the schema could say where that is: status_id
-- says which column a ticket is in and created_at is the only order it has.
--
--
-- The convention
--
-- The same as status.position (0010) and board_column.position (0025): an
-- integer that is an ordering hint rather than a slot, non-unique, read as
-- ORDER BY position, id. Two writers dropping cards into the same gap at the
-- same moment can land on one number, and the id breaks the tie the same way
-- for everybody. A UNIQUE constraint would turn that harmless race into a 409
-- for a drag-and-drop.
--
-- Positions are spread 1024 apart, so a card dropped between two others takes
-- the midpoint and writes one row. When two neighbours have no number between
-- them the service renumbers that column in one statement; see
-- internal/ticket/move.go.
--
-- The scope of a position is the project. A ticket's column is its status, so
-- the board orders the tickets of one status in one project, and the same
-- numbers order the project's whole backlog when nothing is filtered.
--
--
-- The backfill
--
-- Existing tickets are numbered by created_at within their project, oldest
-- first, which is the order every list has shown them in so far. Ties on
-- created_at fall back to id.
--
-- The default of 0 exists so INSERTs that predate this migration keep working.
-- The service always writes a position of its own: the end of the project.

-- +migrate Up

ALTER TABLE ticket ADD COLUMN position integer NOT NULL DEFAULT 0;

UPDATE ticket t
   SET position = r.rn * 1024
  FROM (SELECT id, row_number() OVER (PARTITION BY project_id ORDER BY created_at, id) AS rn
          FROM ticket) r
 WHERE r.id = t.id;

CREATE INDEX ticket_column_order_idx ON ticket (project_id, status_id, position, id);

COMMENT ON COLUMN ticket.position IS
    'Order within a board column: ORDER BY position, id. A hint rather than a slot - not unique, duplicates are legal and the id breaks the tie. Spread 1024 apart. See LAM-85.';

-- +migrate Down

DROP INDEX ticket_column_order_idx;
ALTER TABLE ticket DROP COLUMN position;

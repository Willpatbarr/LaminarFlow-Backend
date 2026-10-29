-- +migrate Up

-- LAM-134: a status sits in one column of a board. The board service enforces that, but
-- DeleteStatus used to re-point a deleted status's column mapping at move_to, which could
-- be mapped to another column of the same board already. Remove the existing duplicates.
--
-- Rule: per board and status, keep the mapping in the column with the lowest position
-- (ties: lowest column id) and delete the others. A column left with no status stays.

DELETE FROM board_column_status bcs
 USING board_column c
 WHERE c.id = bcs.board_column_id
   AND EXISTS (
       SELECT 1
         FROM board_column_status other
         JOIN board_column oc ON oc.id = other.board_column_id
        WHERE other.status_id = bcs.status_id
          AND oc.board_id = c.board_id
          AND other.board_column_id <> bcs.board_column_id
          AND (oc.position, oc.id) < (c.position, c.id));

-- +migrate Down

-- The removed duplicate mappings are not restorable; nothing to undo.
SELECT 1;

-- LAM-137: status names in the activity log.
--
-- A status_changed entry stored only status ids, and a reader resolved names against the
-- statuses it still held. Delete a status and every entry that mentioned it lost its name
-- ("moved this from To Do to another status"). The names are now stored at write time, as
-- a snapshot: a later rename does not rewrite history, which is what the entry said when
-- it happened (Linear does the same).
--
-- name_before / name_after are null for every kind but status_changed, and null on a
-- status_changed entry whose status was already gone when this migration ran: that name
-- cannot be recovered.

-- +migrate Up

ALTER TABLE activity
    ADD COLUMN name_before text,
    ADD COLUMN name_after  text;

UPDATE activity a
   SET name_before = (SELECT s.name FROM status s WHERE s.id::text = a.summary_before),
       name_after  = (SELECT s.name FROM status s WHERE s.id::text = a.summary_after)
 WHERE a.kind = 'status_changed';

-- +migrate Down

ALTER TABLE activity
    DROP COLUMN name_before,
    DROP COLUMN name_after;

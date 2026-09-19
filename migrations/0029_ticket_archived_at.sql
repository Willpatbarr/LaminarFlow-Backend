-- LAM-57: ticket.archived_at.
--
-- The first archive column in this schema, and the first soft delete. Both of
-- those are reasons to be careful rather than reasons not to, so this records
-- the argument in full.
--
--
-- Why archive rather than delete
--
-- API Design notes section 1 writes "Delete / Archive" as though it were one
-- verb. It is two products, and LAM-57 had to pick one.
--
-- The deciding fact is not in that note: comment CASCADEs from ticket (0017).
-- A hard delete therefore takes the entire comment thread with it, silently
-- and unrecoverably. 0011's careful SET NULLs on status_id and
-- assignee_account_id protect a deleted ticket's *neighbours* - work outlives
-- the workflow it sat in and the people who touched it - but nothing in the
-- schema protects a ticket's own history from the ticket being removed.
--
-- Losing a status is an inconvenience. Losing a week of discussion because
-- someone tidied a board is not, and no undo exists for it.
--
--
-- What this costs, stated plainly
--
-- Every read of ticket now carries a predicate, forever. One query that
-- forgets it shows archived tickets on a board, and nothing errors. That is a
-- real and permanent tax, and it is the reason a soft delete is usually the
-- wrong default.
--
-- It is paid down in one place rather than per query: internal/ticket appends
-- the predicate in the service, so no caller writes it and no caller can
-- forget it. LAM-59's list endpoint inherits it from there too.
--
-- Search is the second consumer and the easy one to miss. An archived ticket
-- must not surface in results, so internal/search's ticket insert excludes
-- them - which means archiving a ticket has to reindex it out, not merely set
-- a column.
--
--
-- A column, not a status
--
-- Archiving is deliberately not a status value. status is per-team and
-- user-editable (0010, master spec 3.4 "no status is sacred"), so a team could
-- rename or delete the archive status and break archiving for everyone. A
-- timestamptz column cannot be renamed away, and it records *when*, which a
-- boolean would not.
--
-- Nullable rather than a boolean with a default: null is "not archived", and
-- the timestamp is only meaningful when it exists. That also makes the partial
-- index below possible.

-- +migrate Up

ALTER TABLE ticket ADD COLUMN archived_at timestamptz;

-- Partial, because the overwhelmingly common read is "the tickets that are
-- not archived" and that predicate selects almost every row - an index over
-- the whole column would be ignored. This one indexes only the archived
-- minority, which is what an "archived tickets" view will read.
CREATE INDEX ticket_archived_idx ON ticket (project_id, archived_at)
    WHERE archived_at IS NOT NULL;

COMMENT ON COLUMN ticket.archived_at IS
    'Null means live. Set means archived, and every read of ticket must exclude it - internal/ticket applies that predicate so no caller has to.';

-- +migrate Down

DROP INDEX ticket_archived_idx;
ALTER TABLE ticket DROP COLUMN archived_at;

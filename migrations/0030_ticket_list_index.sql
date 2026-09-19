-- LAM-59: the index the default ticket list reads.
--
-- 0011 built three indexes for the board and the two foreign keys. None of
-- them serves the list endpoint's default order, which is updated_at
-- descending - the order a table view opens in, and therefore the one page 1
-- of every list hits before a caller has chosen anything.
--
-- Without it, page 1 of an empty filter is a sequential scan of ticket plus a
-- sort. That is free today and is not free later, and the deployment target is
-- a Raspberry Pi with a USB disk.
--
--
-- Why (updated_at DESC, id) and not just (updated_at)
--
-- The list is keyset-paginated, and its ORDER BY is
--
--     updated_at DESC NULLS LAST, id
--
-- An index matches an ordering only if its own column order and directions
-- match. A btree on (updated_at) alone can be walked backwards for the sort,
-- but then id is unordered within a group of equal timestamps - which is
-- exactly the case the tiebreak exists for, so the planner would have to sort
-- anyway. Both columns, in the directions the query asks for.
--
-- NULLS LAST is not spelled here because updated_at is NOT NULL, so the
-- clause is a no-op on this column and Postgres matches the index regardless.
-- internal/filter emits it unconditionally, for the nullable columns where it
-- is not a no-op.
--
--
-- Why partial
--
-- Every read of ticket carries archived_at IS NULL (0029). A partial index on
-- that predicate is smaller, and it stays small as archived tickets
-- accumulate - which is the direction that column moves, permanently. The
-- complement is already indexed by ticket_archived_idx.
--
-- This is an index, not a constraint, so it gets no schema constraint test.
-- What it is for is asserted differently: internal/ticket's list tests pin the
-- order and the page boundaries, which is the behaviour this only makes fast.

-- +migrate Up

CREATE INDEX ticket_updated_idx ON ticket (updated_at DESC, id)
    WHERE archived_at IS NULL;

COMMENT ON INDEX ticket_updated_idx IS
    'Serves the default ticket list order (updated_at DESC, id) under the archive predicate. See LAM-59.';

-- +migrate Down

DROP INDEX ticket_updated_idx;

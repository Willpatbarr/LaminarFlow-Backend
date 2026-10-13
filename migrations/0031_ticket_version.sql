-- LAM-65: ticket.version, ticket.updated_by, ticket.archived_by.
--
-- A local-first client edits a ticket, then syncs later. If someone else changed
-- the ticket in between, last-write-wins silently destroys their edit, and the
-- client cannot even say so. Three things were missing to do better, and all three
-- are columns:
--
--   version      a counter the client sends back to say "I edited version 4"
--   updated_by   who wrote the current state, for "edited by Sarah"
--   archived_by  who archived it, for "archived by Sarah" (archived_at has been
--                here since 0029, with nobody attached to it)
--
--
-- Why a counter and not updated_at
--
-- updated_at is a timestamp taken at the start of a transaction. Two writes in the
-- same microsecond share one, and two overlapping transactions can commit in the
-- opposite order to their timestamps. A precondition has to be an exact match on
-- something that changes on every write and never otherwise, and a timestamp is
-- neither. An integer incremented in the same UPDATE as everything else is both.
--
-- It is also what the change feed (a WebSocket broadcast of ticket events) carries,
-- so a client can recognise the echo of its own write by comparing one integer.
--
--
-- The default
--
-- migrations/README.md warns against a DEFAULT that invents an answer the caller is
-- meant to supply. This is not one: every ticket starts at version 1, so 1 is a
-- property of the column rather than of any caller, in the same way created_at's
-- now() is. The default is also what keeps INSERTs that predate this migration
-- working - the expand step of expand-and-contract is complete the moment this
-- applies, and no contract step follows because NOT NULL DEFAULT is already the end
-- state.
--
-- Existing rows therefore become version 1, which is what they would have been had
-- the column always existed: nothing has been written to them since.
--
--
-- Why updated_by and archived_by are nullable
--
-- Two reasons, and neither is laziness.
--
-- Rows written before this migration have no recorded writer, and inventing one
-- would be a lie the UI repeats ("edited by Sarah" about something Sarah never
-- touched). Null means "not recorded" and the API reports it as null.
--
-- And ON DELETE SET NULL is the only action 0011 allows for a reference to an
-- account: work outlives the people who touched it. A ticket whose last editor has
-- since left keeps its row and loses the name, exactly as assignee_account_id does.
-- A client treats null as "someone", never as an error.
--
--
-- The indexes
--
-- Both foreign keys are SET NULL, so by the rule in migrations/README.md both need
-- an index even though no query reads by them: deleting one account would otherwise
-- scan the whole ticket table twice. They are partial on IS NOT NULL because the
-- foreign key's lookup is an equality, which implies not-null, so the planner can
-- still use them - and because archived_by is null for nearly every row, so an
-- index over the whole column would be mostly nothing.
--
-- No index on version. Nothing reads by it: the precondition is checked against a row
-- already found by primary key.

-- +migrate Up

ALTER TABLE ticket
    ADD COLUMN version     integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    ADD COLUMN updated_by  uuid    REFERENCES account(id) ON DELETE SET NULL,
    ADD COLUMN archived_by uuid    REFERENCES account(id) ON DELETE SET NULL;

CREATE INDEX ticket_updated_by_idx ON ticket (updated_by) WHERE updated_by IS NOT NULL;
CREATE INDEX ticket_archived_by_idx ON ticket (archived_by) WHERE archived_by IS NOT NULL;

COMMENT ON COLUMN ticket.version IS
    'Starts at 1 and rises by exactly 1 on every successful update, archive and unarchive, in the same statement as the write. A client sends the version it last saw as If-Match; a mismatch is a 409. See LAM-65.';
COMMENT ON COLUMN ticket.updated_by IS
    'The account that wrote the current state. Null for rows from before 0031, and after that account is deleted.';
COMMENT ON COLUMN ticket.archived_by IS
    'The account that archived the ticket. Null while live, for tickets archived before 0031, and after that account is deleted.';

-- +migrate Down

DROP INDEX ticket_archived_by_idx;
DROP INDEX ticket_updated_by_idx;
ALTER TABLE ticket
    DROP COLUMN archived_by,
    DROP COLUMN updated_by,
    DROP COLUMN version;

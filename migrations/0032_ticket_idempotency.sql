-- LAM-75: ticket.created_by and ticket.idempotency_key.
--
-- A client that creates a ticket and loses the response cannot tell whether the ticket
-- exists. Retrying risks a duplicate; not retrying risks losing the work. The standard
-- answer is a key the client chooses and sends with the create: the same key from the
-- same account returns the ticket the first call made, and the second call writes
-- nothing. These two columns are what that needs.
--
--
-- Why created_by
--
-- A key is only unique per creator. Two accounts picking the same key (a counter, a
-- timestamp, a short random string) must not see each other's tickets, so the unique
-- index is over (created_by, idempotency_key) and not over the key alone. Scoping by
-- anything wider than the creator would also leak: a replay of someone else's key would
-- hand one account another's ticket.
--
-- updated_by cannot stand in for it - it moves on every edit - so the creator needs a
-- column of its own. It is written on every new ticket, not only keyed ones: a column
-- that is set for some inserts and not others is a column nobody can rely on. It is
-- nullable for the same two reasons updated_by is (0031): the tickets that exist
-- already have no recorded creator and inventing one would be a lie, and ON DELETE SET
-- NULL is the only action 0011 allows for a reference to an account - work outlives the
-- people who made it. No backfill, for the first reason.
--
--
-- Why nothing ties the two columns together
--
-- The tempting CHECK is "a key requires a creator". It would make every account with
-- a keyed ticket undeletable: ON DELETE SET NULL nulls created_by, the CHECK refuses the
-- resulting row, and the delete fails. migrations/README.md warns about this shape
-- ("A CHECK can take an ON DELETE action away from you"). Without the CHECK a deleted
-- creator leaves (NULL, key) behind, which the unique index ignores (NULLs are
-- distinct), and nobody can replay the key anyway - the account is gone.
--
--
-- The indexes
--
-- ticket_creator_idempotency_key_idx is the one the feature needs. It is partial on
-- idempotency_key IS NOT NULL: nearly every ticket has no key, and an index entry for
-- each of them would be mostly nothing. The create statement names the same predicate in
-- its ON CONFLICT clause, which is how Postgres knows to infer this index.
--
-- Being partial, it cannot serve the foreign key on created_by - the planner cannot
-- prove a lookup by created_by only touches rows with a key. And ON DELETE SET NULL
-- needs an index even when no query does (migrations/README.md), or deleting one account
-- scans the whole ticket table. So created_by gets its own, partial on IS NOT NULL as
-- 0031's are: the foreign key's lookup is an equality, which implies not-null.
--
--
-- The shape of a key
--
-- Printable ASCII, 1 to 128 characters. The API refuses anything else with a 422 before
-- it reaches here; the CHECK repeats the rule so a second writer cannot store a key no
-- client could send. It is safe to add: the column is new, so no existing row has a
-- value to violate it.
--
-- No expiry. Keys live as long as their ticket. A later ticket may add a retention
-- window; it would be a cleanup of this column, not a change to its meaning.

-- +migrate Up

ALTER TABLE ticket
    ADD COLUMN created_by      uuid REFERENCES account(id) ON DELETE SET NULL,
    ADD COLUMN idempotency_key text,
    ADD CONSTRAINT ticket_idempotency_key_shape
        CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[ -~]{1,128}$');

CREATE UNIQUE INDEX ticket_creator_idempotency_key_idx
    ON ticket (created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX ticket_created_by_idx ON ticket (created_by) WHERE created_by IS NOT NULL;

COMMENT ON COLUMN ticket.created_by IS
    'The account that created the ticket. Null for tickets from before 0032, and after that account is deleted. Never changes while the account exists.';
COMMENT ON COLUMN ticket.idempotency_key IS
    'The Idempotency-Key the creating request carried, unique per created_by. Null for tickets created without one. Never expires. See LAM-75.';

-- +migrate Down

DROP INDEX ticket_created_by_idx;
DROP INDEX ticket_creator_idempotency_key_idx;
ALTER TABLE ticket
    DROP CONSTRAINT ticket_idempotency_key_shape,
    DROP COLUMN idempotency_key,
    DROP COLUMN created_by;

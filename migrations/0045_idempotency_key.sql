-- LAM-127: one shared idempotency_key table for every create that has no key column of its own.
--
-- 0032 (ticket), 0039 (document), 0040 (comment and review request) and 0044 (attachment)
-- each put the key on the resource's own row, under a partial unique index, and the create
-- statement is INSERT ... ON CONFLICT. That is the best shape when the table is already
-- the unit of work and the key is one more column. It does not scale to the other thirteen
-- creates (team, project, status, epic, sprint, milestone, label, board, board column,
-- saved view, aspect type, aspect type import, aspect field): thirteen more columns, thirteen
-- more indexes, and for status, column and field a parent-scoped row where "the operation"
-- is not even one table. They share this table instead. The existing five keep their columns
-- and their tests; nothing here touches them.
--
--
-- The shape
--
-- One row per (account, key, operation). `operation` is the operation id of the create
-- ('create-epic', 'import-aspect-type', ...), so one key may be used once per kind of create
-- without the kinds colliding, and a retry that lands on the wrong endpoint cannot return a
-- different kind of resource. resource_id is the id of what the first call made. It is not a
-- foreign key: it points at thirteen tables.
--
--
-- Claim, create, record
--
-- The first request with a key INSERTs the row with resource_id NULL (a claim, committed on
-- its own), creates the resource, then sets resource_id. A concurrent request with the same
-- key finds the claim and waits for resource_id; there is no check-then-insert, so sixteen at
-- once make one resource. A create that fails deletes its claim, so the retry can try again.
-- A claim that never gets a resource_id (the process died between the claim and the record)
-- is taken over after claimed_at is more than 30 seconds old. The cost of that crash window
-- is one orphaned resource, which is the same cost the client would have paid with no key.
--
--
-- Deleted resources
--
-- Like comments (0040), these resources are hard-deleted. When the replay finds the recorded
-- resource gone, the stale row is dropped and the request creates afresh: a retry cannot tell
-- a lost response from a deleted resource, and the resource it would return no longer exists.
--
--
-- Why account_id is a real foreign key
--
-- Unlike created_by on ticket, there is no row here worth keeping once the account goes, and
-- a key is only meaningful to the account that chose it. ON DELETE CASCADE removes them with
-- it, and the primary key (account_id, key, operation) is led by account_id, which serves the
-- cascade, so no second index.
--
--
-- The shape of a key
--
-- Printable ASCII, 1 to 128 characters, the same CHECK as the other five.

-- +migrate Up

CREATE TABLE idempotency_key (
    account_id  uuid        NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    key         text        NOT NULL,
    operation   text        NOT NULL,
    resource_id uuid,
    claimed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, key, operation),
    CONSTRAINT idempotency_key_shape CHECK (key ~ '^[ -~]{1,128}$')
);

COMMENT ON TABLE idempotency_key IS
    'Idempotency-Key claims for the creates that have no key column of their own. See LAM-127 and docs/api-conventions.md section 10.';
COMMENT ON COLUMN idempotency_key.operation IS
    'The operation id of the create, e.g. create-epic. A key is unique per account and operation.';
COMMENT ON COLUMN idempotency_key.resource_id IS
    'The id of what the first call created. Null while that call is in flight (a claim). Not a foreign key: it points at many tables.';

-- +migrate Down

DROP TABLE idempotency_key;

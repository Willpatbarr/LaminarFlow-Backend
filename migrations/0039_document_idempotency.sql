-- LAM-126: document.created_by and document.idempotency_key.
--
-- POST /documents gets the Idempotency-Key behaviour POST /tickets has had since 0032
-- (LAM-75): the same key from the same account returns the document the first call made,
-- and the second call writes nothing. Everything below is 0032's reasoning applied to
-- document; read that file for the long version. Only what differs is said at length here.
--
--
-- Why created_by
--
-- A key is only unique per creator, so the unique index is over (created_by,
-- idempotency_key) and not over the key alone: two accounts choosing the same key must
-- not see each other's documents. created_by is nullable for 0032's two reasons: the
-- documents that exist already have no recorded creator and inventing one would be a lie
-- (no backfill), and ON DELETE SET NULL is the only action 0011 allows for a reference to
-- an account. It is written on every new document made on behalf of an account, not only
-- keyed ones; a document written with no account (the service's WorkspaceID-only callers)
-- has none.
--
--
-- Why nothing ties the two columns together
--
-- No CHECK that "a key requires a creator": it would make an account with a keyed document
-- undeletable (migrations/README.md, "A CHECK can take an ON DELETE action away from you").
-- A deleted creator leaves (NULL, key), which the unique index ignores.
--
--
-- The indexes
--
-- document_creator_idempotency_key_idx is partial on idempotency_key IS NOT NULL, because
-- nearly every document has no key; the create statement names the same predicate in its
-- ON CONFLICT clause. document_created_by_idx exists for the foreign key's ON DELETE SET
-- NULL, which the partial index cannot serve.
--
--
-- The shape of a key
--
-- Printable ASCII, 1 to 128 characters, the same CHECK as ticket's. The API refuses
-- anything else with a 422 first; the CHECK stops a second writer storing a key no client
-- could send. Safe to add: the column is new. No expiry; keys live as long as the document.

-- +migrate Up

ALTER TABLE document
    ADD COLUMN created_by      uuid REFERENCES account(id) ON DELETE SET NULL,
    ADD COLUMN idempotency_key text,
    ADD CONSTRAINT document_idempotency_key_shape
        CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[ -~]{1,128}$');

CREATE UNIQUE INDEX document_creator_idempotency_key_idx
    ON document (created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX document_created_by_idx ON document (created_by) WHERE created_by IS NOT NULL;

COMMENT ON COLUMN document.created_by IS
    'The account that created the document. Null for documents from before 0039, for writes made with no account, and after that account is deleted. Never changes while the account exists.';
COMMENT ON COLUMN document.idempotency_key IS
    'The Idempotency-Key the creating request carried, unique per created_by. Null for documents created without one. Never expires. See LAM-126.';

-- +migrate Down

DROP INDEX document_created_by_idx;
DROP INDEX document_creator_idempotency_key_idx;
ALTER TABLE document
    DROP CONSTRAINT document_idempotency_key_shape,
    DROP COLUMN idempotency_key,
    DROP COLUMN created_by;

-- LAM-99: comment.idempotency_key.
--
-- POST /comments gets the Idempotency-Key behaviour POST /tickets (0032, LAM-75) and POST
-- /documents (0039, LAM-126) have: the same key from the same account returns the comment the
-- first call made, and the second call writes nothing. The frontend outbox sends one with every
-- queued create. Read 0032 for the long reasoning; only what differs is said here.
--
--
-- No created_by column
--
-- ticket and document needed one because nothing else recorded their creator. A comment already
-- has author_id (0017), nullable and ON DELETE SET NULL, which is exactly the shape 0032 gave
-- created_by: set on every comment made on behalf of an account, nulled when that account is
-- deleted, never changed otherwise. The key is scoped by it, so the unique index is over
-- (author_id, idempotency_key). An authorless comment has (NULL, key), which the index ignores,
-- and nobody can replay that key anyway - the account is gone.
--
--
-- Why nothing ties the two columns together
--
-- No CHECK that "a key requires an author": it would make an account with a keyed comment
-- undeletable (migrations/README.md, "A CHECK can take an ON DELETE action away from you").
--
--
-- The index
--
-- Partial on idempotency_key IS NOT NULL, because nearly every comment has no key; the create
-- statement names the same predicate in its ON CONFLICT clause. author_id already has
-- comment_author_idx (0017) for the foreign key's ON DELETE SET NULL, so no second index.
--
--
-- Keys outlive nothing but their comment
--
-- Comments are hard-deleted (DELETE /comments/{id}), so deleting one frees its key: a retry of
-- the original create after the delete makes a new comment. That is the honest answer for a
-- resource with no archive state; a retry cannot tell a lost response from a deleted comment
-- and the comment it would have returned no longer exists.
--
--
-- The shape of a key
--
-- Printable ASCII, 1 to 128 characters, the same CHECK as ticket's and document's.

-- +migrate Up

ALTER TABLE comment
    ADD COLUMN idempotency_key text,
    ADD CONSTRAINT comment_idempotency_key_shape
        CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[ -~]{1,128}$');

CREATE UNIQUE INDEX comment_author_idempotency_key_idx
    ON comment (author_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMENT ON COLUMN comment.idempotency_key IS
    'The Idempotency-Key the creating request carried, unique per author_id. Null for comments created without one. Lives as long as the comment. See LAM-99.';

-- +migrate Down

DROP INDEX comment_author_idempotency_key_idx;
ALTER TABLE comment
    DROP CONSTRAINT comment_idempotency_key_shape,
    DROP COLUMN idempotency_key;

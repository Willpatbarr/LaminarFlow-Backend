-- LAM-90: the attachment table - the metadata of an uploaded file. The bytes live in
-- internal/storage (a folder on a volume today), under storage_key.
--
--
-- The owner is an exclusive arc, the same as comment (0017)
--
-- Exactly one of ticket_id and document_id, each with a real foreign key and ON DELETE
-- CASCADE: an attachment on a deleted ticket is not a record of anything. Read 0017 for
-- why this is not a target_type/target_id pair. The cascade removes the row, not the file;
-- internal/attachment documents the orphan that can leave in the folder.
--
--
-- storage_key is generated, never derived from the filename
--
-- A uuid chosen by the server. filename is what the uploader called the file, kept only
-- to name the download; nothing reads it as a path. UNIQUE so two rows can never share a
-- blob, which would make deleting one destroy the other's bytes.
--
--
-- created_by is nullable and SET NULL, for LAM-18's reason
--
-- Work outlives the people who did it. Readers must render an attachment with no uploader.
--
--
-- idempotency_key
--
-- POST /attachments takes Idempotency-Key like the other creates (0032, 0039, 0040). Read
-- 0040 for the reasoning; it is identical, with created_by in the role author_id plays
-- there. A retry whose key has been used returns the first attachment and stores nothing.
--
--
-- content_type is the type the server decided, not the type the client claimed
--
-- Chosen from the file's own leading bytes and a short allowlist (internal/attachment),
-- so it is safe to serve back. No CHECK: the list will grow, and a CHECK that has to be
-- migrated to allow a new file type is friction with no safety behind it - the allowlist
-- is enforced where the value is chosen.

-- +migrate Up

CREATE TABLE attachment (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    ticket_id       uuid        REFERENCES ticket(id)   ON DELETE CASCADE,
    document_id     uuid        REFERENCES document(id) ON DELETE CASCADE,

    filename        text        NOT NULL,
    content_type    text        NOT NULL,
    size_bytes      bigint      NOT NULL,
    storage_key     text        NOT NULL UNIQUE,
    created_by      uuid        REFERENCES account(id)  ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    idempotency_key text,

    CONSTRAINT attachment_exactly_one_owner
        CHECK (num_nonnulls(ticket_id, document_id) = 1),

    CONSTRAINT attachment_size_is_not_negative
        CHECK (size_bytes >= 0),

    CONSTRAINT attachment_idempotency_key_shape
        CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[ -~]{1,128}$')
);

-- Every attachment of one owner, split in two because the owner is; each also serves its
-- own CASCADE. Partial, because exactly one of the two is null on every row. created_at
-- first so the list's order and its keyset cursor walk the index.
CREATE INDEX attachment_ticket_idx   ON attachment (ticket_id, created_at, id)   WHERE ticket_id   IS NOT NULL;
CREATE INDEX attachment_document_idx ON attachment (document_id, created_at, id) WHERE document_id IS NOT NULL;

-- created_by is SET NULL, so it needs an index for the foreign key whether or not a read wants it.
CREATE INDEX attachment_created_by_idx ON attachment (created_by) WHERE created_by IS NOT NULL;

CREATE UNIQUE INDEX attachment_creator_idempotency_key_idx
    ON attachment (created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

COMMENT ON TABLE attachment IS
    'The metadata of an uploaded file on a ticket or a document. The bytes are in internal/storage under storage_key. See LAM-90.';

COMMENT ON COLUMN attachment.filename IS
    'The uploader''s name for the file, sanitised. Only ever used to name a download; never a path.';

COMMENT ON COLUMN attachment.storage_key IS
    'A server-generated uuid naming the blob in storage. Never derived from filename.';

COMMENT ON COLUMN attachment.created_by IS
    'Nullable and SET NULL: deleting an account keeps the file and loses the attribution.';

-- +migrate Down

DROP TABLE attachment;

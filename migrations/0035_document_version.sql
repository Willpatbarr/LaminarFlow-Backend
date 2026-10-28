-- LAM-87: document.version and document.archived_at.
--
-- The document API needs the same two things the ticket API got in 0029 and 0031, for the
-- same reasons, so this migration says less than they do. Read those for the argument.
--
--   version      a counter a client sends back as If-Match, so an editor that was offline
--                is told "someone else edited this" instead of silently overwriting it.
--                A counter and not updated_at, for 0031's reason: a timestamp can tie or
--                commit out of order, and a precondition must be an exact match on something
--                that changes on every write and never otherwise.
--   archived_at  DELETE archives. A document is the work itself, and comment CASCADEs from
--                its parent (0017), so a hard delete would take the discussion with it.
--
-- Not added: updated_by and archived_by. 0031 added them so a 409 could say "edited by
-- Sarah". Nothing asks for that on a document yet, and a column nobody reads is a column
-- somebody has to keep correct.
--
-- The version default is 1 for the reason 0031 gives: every document starts at 1, so 1 is a
-- property of the column and not an answer a caller was meant to supply. Existing rows become
-- version 1, which is what they would have been had the column always existed.
--
-- archived_at is nullable: null is "not archived". The partial index covers only the archived
-- minority, which is what an "archived documents" list reads; the common read is the live
-- majority, which an index over the column would not narrow.

-- +migrate Up

ALTER TABLE document
    ADD COLUMN version     integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    ADD COLUMN archived_at timestamptz;

CREATE INDEX document_archived_idx ON document (archived_at) WHERE archived_at IS NOT NULL;

COMMENT ON COLUMN document.version IS
    'Starts at 1 and rises by exactly 1 on every successful update, archive and unarchive, in the same statement as the write. A client sends the version it last saw as If-Match; a mismatch is a 409. See LAM-87.';
COMMENT ON COLUMN document.archived_at IS
    'Null while live. Set by archive, which is what DELETE /documents/{id} does. An archived document is not read, listed or searched.';

-- +migrate Down

DROP INDEX document_archived_idx;
ALTER TABLE document
    DROP COLUMN archived_at,
    DROP COLUMN version;

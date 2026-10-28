-- LAM-111: ticket_document, the many-to-many between tickets and documents.
--
-- A ticket links to the documents that explain it, and a document to the tickets it is about.
-- The relation is symmetric - nothing is "the" parent - so it is a join table of its own
-- rather than a column on either side. internal/ticket owns it (see boundary_test.go);
-- internal/document reads the tickets of a document through a ticket service method.
--
--
-- Both foreign keys CASCADE
--
-- A link means nothing without both ends, so deleting either row deletes the link. This is
-- about hard deletes only. Archiving is a soft delete (ticket 0029, document 0035) and does
-- not touch this table: an archived ticket and an archived document keep their links, so
-- unarchiving restores the picture exactly as it was.
--
--
-- Why created_by is nullable
--
-- The same two reasons as ticket.created_by (0032) and updated_by (0031). Nothing before this
-- migration recorded who linked what, and there is nothing to backfill because the table is
-- new; and ON DELETE SET NULL is the only action 0011 allows for a reference to an account -
-- work outlives the people who did it. A link whose author has left keeps its row and loses
-- the name. Null means "someone", never an error.
--
--
-- The indexes
--
-- The primary key (ticket_id, document_id) serves "documents of a ticket" and the idempotent
-- insert. It cannot serve "tickets of a document", and it cannot serve the cascade from
-- document: deleting one document would scan the whole table. So document_id gets its own
-- index (migrations/README.md: every foreign key that cascades needs one).
--
-- created_by is SET NULL, so by the same rule it needs an index even though no query reads by
-- it. It is partial on IS NOT NULL because the foreign key's lookup is an equality, which
-- implies not-null (0031, 0032).

-- +migrate Up

CREATE TABLE ticket_document (
    ticket_id   uuid        NOT NULL REFERENCES ticket(id)   ON DELETE CASCADE,
    document_id uuid        NOT NULL REFERENCES document(id) ON DELETE CASCADE,
    created_by  uuid        REFERENCES account(id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, document_id)
);

CREATE INDEX ticket_document_document_id_idx ON ticket_document (document_id);
CREATE INDEX ticket_document_created_by_idx ON ticket_document (created_by) WHERE created_by IS NOT NULL;

COMMENT ON TABLE ticket_document IS
    'A link between a ticket and a document, symmetric. Written only by internal/ticket. Archiving either end does not remove it; deleting either end does. See LAM-111.';
COMMENT ON COLUMN ticket_document.created_by IS
    'The account that made the link. Null after that account is deleted.';

-- +migrate Down

DROP INDEX ticket_document_created_by_idx;
DROP INDEX ticket_document_document_id_idx;
DROP TABLE ticket_document;

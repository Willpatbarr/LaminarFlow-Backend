-- LAM-89: the activity log. One row per meaningful change to a ticket or a document,
-- written in the same transaction as the change.
--
--
-- The entity is an exclusive arc
--
-- ticket_id / document_id, exactly one set, each with a real foreign key and ON DELETE
-- CASCADE: the pattern 0017 chose for comment, for the reason it gives. A polymorphic
-- entity_type/entity_id pair could carry no foreign key and would leave rows pointing at
-- nothing. The API still speaks entity_type and entity_id; the arc is storage.
--
--
-- actor_id is nullable and SET NULL
--
-- As comment.author_id (0017): work outlives the people who touched it. A change made by
-- an API token is recorded as the account the token acts for - tokens authenticate as
-- their owner and no service below the API sees the token. A document write made with no
-- account (document.Save with only a workspace) has no actor and stores NULL.
--
--
-- kind is a closed set, and a CHECK
--
-- Every reader branches on it, so a typo would drop a row out of every rendering rather than
-- fail - the argument 0017 makes for review_status. Adding a kind costs a migration, which
-- is the point. Document rows may only use the kinds a document can have.
--
--
-- field, and why it is not in the ticket's column list
--
-- field_changed must say which field. Packing it into summary_before would make a reader
-- parse it back out, so it is a column, required for field_changed and null otherwise.
-- Values: ticket - title, description, milestone; document - title, fields.
--
--
-- summary_before / summary_after
--
-- Text, not references: status_changed, assignee_changed, parent_changed, label_*,
-- aspect_type_changed carry the id of the thing before and after (null for none, so a
-- stored value survives the thing being renamed or deleted - the reader resolves names
-- against what it already holds). title and description carry the text, cut to 200
-- characters. A document's 'fields' carries the sorted field ids. Not full diffs.
--
--
-- Indexes
--
-- The only read is "this entity's history, newest first", so (entity, created_at), one
-- partial index per arm of the arc. They also serve the CASCADE.

-- +migrate Up

CREATE TABLE activity (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id      uuid        REFERENCES ticket(id)   ON DELETE CASCADE,
    document_id    uuid        REFERENCES document(id) ON DELETE CASCADE,
    actor_id       uuid        REFERENCES account(id)  ON DELETE SET NULL,
    kind           text        NOT NULL,
    field          text,
    summary_before text,
    summary_after  text,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT activity_exactly_one_entity
        CHECK (num_nonnulls(ticket_id, document_id) = 1),

    CONSTRAINT activity_kind_is_known
        CHECK (kind IN ('created', 'status_changed', 'assignee_changed', 'parent_changed',
                        'label_added', 'label_removed', 'archived', 'unarchived',
                        'aspect_type_changed', 'field_changed')),

    -- A document has no status, assignee, parent or labels.
    CONSTRAINT activity_document_kind
        CHECK (document_id IS NULL
               OR kind IN ('created', 'archived', 'unarchived', 'aspect_type_changed', 'field_changed')),

    -- Which field, exactly when it is a field change.
    CONSTRAINT activity_field_matches_kind
        CHECK ((kind = 'field_changed') = (field IS NOT NULL))
);

CREATE INDEX activity_ticket_idx   ON activity (ticket_id, created_at)   WHERE ticket_id   IS NOT NULL;
CREATE INDEX activity_document_idx ON activity (document_id, created_at) WHERE document_id IS NOT NULL;

-- SET NULL needs an index to find the rows, whether or not a read wants one.
CREATE INDEX activity_actor_idx ON activity (actor_id) WHERE actor_id IS NOT NULL;

COMMENT ON TABLE activity IS
    'Append-only history of tickets and documents, written by internal/activity in the transaction of the change it describes.';

COMMENT ON COLUMN activity.actor_id IS
    'Nullable and SET NULL. The account behind the change (a token acts as its owner); null when the account is gone or the write had none.';

-- +migrate Down

DROP TABLE activity;

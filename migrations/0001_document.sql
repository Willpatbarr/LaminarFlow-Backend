-- LAM-3 step 1: the document table.
--
-- body is the source of truth for a document's field data: one JSON object
-- keyed by field ID. The search_index table (0002) is derived from this
-- and is disposable. This table is not.

-- +migrate Up

CREATE TABLE document (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    body       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- body maps field ID -> value. A scalar or array here means something
    -- wrote a shape the indexer can't walk, so reject it at the boundary.
    CONSTRAINT document_body_is_object CHECK (jsonb_typeof(body) = 'object')
);

-- +migrate Down

DROP TABLE document;

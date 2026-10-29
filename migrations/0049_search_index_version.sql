-- LAM-143: record which format the search index text is in.
--
-- The index used to hold raw Markdown; it now holds plain text. Rows written before
-- the change keep the old text until rebuilt, and an operator running cmd/reindex by
-- hand after a deploy is a step that gets missed. So the index carries a version, and
-- the server rebuilds it once on startup when the version here is older than the one
-- in the code (search.IndexVersion). Bump the constant, not this table, the next time
-- the format changes.
--
-- One row, enforced: the primary key is a boolean that can only be true. It starts at
-- 0, so the first start after this migration rebuilds once.

-- +migrate Up

CREATE TABLE search_index_version (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    version   integer NOT NULL
);

INSERT INTO search_index_version (singleton, version) VALUES (true, 0);

-- +migrate Down

DROP TABLE search_index_version;

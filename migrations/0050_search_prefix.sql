-- LAM-157: search matches the start of a word as well as stemmed whole words.
--
-- The english tsvectors stem on the way in, so "phrasing" is stored as "phras", and a
-- person who has typed "phrasin" so far matches nothing, however the query is built:
-- no stem of the typed word is a prefix of the stored one. Prefix matching needs the
-- words as they were written, so each tsvector gets a twin built with the `simple`
-- configuration (lowercased, not stemmed, no stop words), each with its own GIN index.
-- The query ORs the two and ranks whole-word matches above prefix-only ones.
--
-- Adding stored generated columns rewrites search_index; the table is derived and small.

-- +migrate Up

ALTER TABLE search_index
    ADD COLUMN search_simple tsvector
        GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
    ADD COLUMN title_search_simple tsvector
        GENERATED ALWAYS AS (to_tsvector('simple', title_or_preview)) STORED;

CREATE INDEX search_index_search_simple_idx ON search_index USING gin (search_simple);
CREATE INDEX search_index_title_search_simple_idx ON search_index USING gin (title_search_simple);

COMMENT ON COLUMN search_index.search_simple IS
    'Generated from content with the simple configuration (no stemming), for prefix matching on the last word typed. See LAM-157.';
COMMENT ON COLUMN search_index.title_search_simple IS
    'Generated from title_or_preview with the simple configuration, for prefix matching. See LAM-157.';

-- +migrate Down

DROP INDEX search_index_title_search_simple_idx;
DROP INDEX search_index_search_simple_idx;
ALTER TABLE search_index DROP COLUMN title_search_simple, DROP COLUMN search_simple;

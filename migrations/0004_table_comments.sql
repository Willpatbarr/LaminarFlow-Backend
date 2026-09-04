-- LAM-10: the first migration applied by the runner rather than by hand.
--
-- Deliberately trivial. Its job is to prove the pipeline end to end - up
-- applies it, down reverses it, schema_migrations records it - without
-- risking anything if the runner is wrong. Table comments are the smallest
-- change that is genuinely reversible: no data moves, no constraint tightens,
-- and `\d+ document` shows the result.
--
-- The content is worth having on its own. Anyone opening psql before reading
-- the Go sees which of these two tables is the source of truth.

-- +migrate Up

COMMENT ON TABLE document IS
    'Source of truth for document field data. body is one JSON object keyed by field ID.';

COMMENT ON TABLE search_index IS
    'Derived from document.body and disposable. Rebuildable via cmd/reindex; never write it directly.';

-- +migrate Down

COMMENT ON TABLE document IS NULL;

COMMENT ON TABLE search_index IS NULL;

-- LAM-100: projects become a search source, and titles become searchable on their own.
--
-- Projects get their own source column
--
-- search_index.project_id is the scope column: "this row lives under that project".
-- A project row needs a source column that says "this row describes that project", and
-- it is a different fact. A project's own row is not scoped under a project, and reusing
-- project_id would make a ticket and a project look the same to every scope filter.
-- So the fourth arm of the exclusive arc is project_source_id (the name keeps it from
-- being read as scope), with the same ON DELETE CASCADE the other three have: delete a
-- project and its search row goes with it.
--
--
-- Titles get their own tsvector
--
-- The generated `search` column covers content only. For a ticket that is enough, since
-- content starts with the title. For a document it is not: content is one field's value,
-- so a document whose title matches and whose fields do not would never be found, and
-- "jump to a known item" is mostly typing a title. title_search is generated from
-- title_or_preview the same way, with its own GIN index, and the search query ORs the two
-- so each index serves its half.
--
-- A document with no fields still gets a row
--
-- 0019 made a document row carry a field_id (a biconditional), so a document whose body
-- is empty - a new, title-only document, the commonest thing to jump to - had no index
-- row at all and could not be found by title. The biconditional is relaxed to one
-- direction: a field_id still needs a document, but a document row may have no field.
-- Such a row is the document's title row, written only when the body is empty (content
-- is the title), so a document has either field rows or one title row, never both.
--
-- Rebuilding the generated column set rewrites search_index. The table is derived and
-- small, and adding a stored generated column is the only way to add one.

-- +migrate Up

ALTER TABLE search_index
    ADD COLUMN project_source_id uuid REFERENCES project(id) ON DELETE CASCADE;

ALTER TABLE search_index DROP CONSTRAINT search_index_exactly_one_source;
ALTER TABLE search_index
    ADD CONSTRAINT search_index_exactly_one_source
        CHECK (num_nonnulls(ticket_id, document_id, comment_id, project_source_id) = 1);

ALTER TABLE search_index DROP CONSTRAINT search_index_document_rows_carry_a_field;
ALTER TABLE search_index
    ADD CONSTRAINT search_index_field_belongs_to_document
        CHECK (field_id IS NULL OR document_id IS NOT NULL);

CREATE INDEX search_index_project_source_idx
    ON search_index (project_source_id) WHERE project_source_id IS NOT NULL;

ALTER TABLE search_index
    ADD COLUMN title_search tsvector
        GENERATED ALWAYS AS (to_tsvector('english', title_or_preview)) STORED;

CREATE INDEX search_index_title_search_idx ON search_index USING gin (title_search);

COMMENT ON COLUMN search_index.project_source_id IS
    'The project this row describes, for a project source. Not the scope column: project_id says which project a row lives under, this says which project it is. See LAM-100.';
COMMENT ON COLUMN search_index.title_search IS
    'Generated from title_or_preview with the english configuration, so a title matches without its content matching. Ranked above content by the search query.';

-- +migrate Down

DELETE FROM search_index WHERE document_id IS NOT NULL AND field_id IS NULL;
ALTER TABLE search_index DROP CONSTRAINT search_index_field_belongs_to_document;
ALTER TABLE search_index
    ADD CONSTRAINT search_index_document_rows_carry_a_field
        CHECK ((document_id IS NOT NULL) = (field_id IS NOT NULL));

DROP INDEX search_index_title_search_idx;
ALTER TABLE search_index DROP COLUMN title_search;

DELETE FROM search_index WHERE project_source_id IS NOT NULL;

DROP INDEX search_index_project_source_idx;
ALTER TABLE search_index DROP CONSTRAINT search_index_exactly_one_source;
ALTER TABLE search_index
    ADD CONSTRAINT search_index_exactly_one_source
        CHECK (num_nonnulls(ticket_id, document_id, comment_id) = 1);
ALTER TABLE search_index DROP COLUMN project_source_id;

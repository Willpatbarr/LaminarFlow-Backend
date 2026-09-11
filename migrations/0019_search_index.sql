-- LAM-26: widening search_index from documents to everything searchable.
--
-- 0002 built this table under LAM-3 as (document_id, field_id, content) with
-- a composite primary key. It is derived and disposable - the document blobs
-- are the source of truth and cmd/reindex regenerates the whole thing - so
-- reshaping it is cheap in a way that reshaping a real table is not.
--
-- This is an ALTER rather than a DROP and CREATE all the same. The rows are
-- rebuildable, but "rebuildable" means an operator has to remember to run
-- cmd/reindex, and a migration that silently empties search until someone
-- does is a worse deploy than one that carries the rows across. Every
-- existing row is a document field row, which is exactly one shape of the
-- new table, so the carry-across is a backfill rather than a translation.
--
--
-- The source is an exclusive arc, not source_type/source_id
--
-- Third time this epic has faced the question, third time with the same
-- answer - LAM-16 for setting's scope, LAM-24 for comment's target, here for
-- the search source. Postgres cannot point one foreign key at three tables,
-- so source_type/source_id would mean no reference on any source, and no
-- ON DELETE. Delete a ticket and its search rows stay, matching queries and
-- pointing at nothing. For a derived table that is worse than usual, because
-- the wrongness is invisible: search results that 404 when clicked, with no
-- constraint violated and nothing to join against to find them.
--
-- LAM-26 lists four source types and this builds three columns, because one
-- of the four is not a thing that exists. An "aspect_field" is not a row
-- anywhere - it is a key inside document.body, and the field definition it
-- names lives in aspect_type_field. The existing table already represents it
-- correctly as (document_id, field_id), and that is preserved: a document
-- source always carries a field_id, so every document row IS an aspect-field
-- row. Collapsing the two removes a source_type value that could never have
-- been resolved to anything.
--
--
-- Scope carries workspace_id, which LAM-26 does not ask for
--
-- The ticket asks for "team_id or project_id (for scoping results)". Both are
-- here and both are nullable, because a document can be workspace-level with
-- neither - that is what 0016 settled. But a search scoped to nothing is not
-- a search anyone runs: the first question every query asks is "within my
-- workspace", and without the column that is a three-table join on the hot
-- path of the feature this table exists to make fast.
--
-- So workspace_id is NOT NULL and the other two narrow it. All three are
-- denormalised copies, which is the entire point of a derived index, and all
-- three are CASCADE because a search row for a deleted workspace is not a
-- search row.
--
--
-- field_id stays text, deliberately, and this is the decision LAM-26 was
-- asked to make rather than inherit.
--
-- The note added to this ticket under LAM-22 said it plainly:
-- aspect_type_field.id is uuid, this column is text, so no foreign key can
-- join them and nothing enforces that an indexed key names a field that
-- exists. Closing it means retyping this column to uuid.
--
-- Not done here, and not for lack of nerve. The blocker is upstream: nothing
-- validates that a key in document.body is an aspect_type_field id. Service
-- .Save accepts any string, the test fixtures use keys like 'f_title', and
-- document.aspect_type_id is nullable so a plain document has no field set to
-- validate against at all. Retyping this column would make the write path
-- reject every body those callers write, which is a breaking change to the
-- document API dressed up as an index migration.
--
-- The order it has to happen in: something validates body keys at the
-- boundary (LAM-44 owns the aspect editor and is the natural home), then the
-- bodies in the wild are clean, then this column can be retyped and the
-- foreign key added. Recorded here so the sequence is not rediscovered.
--
--
-- The full-text column is generated rather than an expression index
--
-- LAM-26 step 2 asks for a tsvector/GIN index on content. A GIN index
-- directly on to_tsvector('english', content) would work, but a stored
-- generated column is the better shape: the vector is visible, can be
-- selected for ranking and highlighting without recomputing it, and cannot
-- drift from content because Postgres owns it. Same technique 0018 used for
-- comment.is_review_request.
--
-- 'english' is hardcoded, and that is a real limitation rather than a
-- decision. Per-language search needs a regconfig column and a different
-- index, and LAM-26 asks for neither. Stated so the next person knows it was
-- seen.
--
--
-- What this migration does NOT do
--
-- LAM-26 steps 3 and 4 ask for the write path and the rebuild to cover
-- tickets and comments, not just documents. They are not here, and this is
-- the largest omission in the epic so far.
--
-- The obstacle is ownership, not effort. internal/document owns every write
-- to search_index, enforced by TestNoSQLOutsideThisPackage, and that rule is
-- correct while the index is a document's derived data. Once it indexes
-- tickets and comments too it is nobody's derived data in particular, and the
-- write path belongs in a package that does not exist - internal/search, with
-- document, ticket and comment services calling into it and the boundary test
-- rewritten to guard the new owner.
--
-- That is a refactor with a design in it, and it is application work in a
-- schema epic. The columns are here and correct; the writers for two of the
-- three sources are filed separately. The document writer is updated in this
-- change because the table shape moved underneath it and it would not
-- otherwise compile.

-- +migrate Up

ALTER TABLE search_index
    ADD COLUMN id               uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN ticket_id        uuid REFERENCES ticket(id)     ON DELETE CASCADE,
    ADD COLUMN comment_id       uuid REFERENCES comment(id)    ON DELETE CASCADE,
    ADD COLUMN workspace_id     uuid REFERENCES workspace(id)  ON DELETE CASCADE,
    ADD COLUMN project_id       uuid REFERENCES project(id)    ON DELETE CASCADE,
    ADD COLUMN team_id          uuid REFERENCES team(id)       ON DELETE CASCADE,
    ADD COLUMN title_or_preview text NOT NULL DEFAULT '',
    ADD COLUMN parent_reference text;

-- The old primary key has to go before document_id and field_id can become
-- nullable: Postgres refuses to drop NOT NULL from a column that is still
-- part of a primary key, with 42P16. Order matters here and the first draft
-- of this migration got it wrong.
ALTER TABLE search_index DROP CONSTRAINT search_index_pkey;

-- document_id and field_id were the primary key and so NOT NULL. They become
-- one arm of the arc.
ALTER TABLE search_index ALTER COLUMN document_id DROP NOT NULL;
ALTER TABLE search_index ALTER COLUMN field_id    DROP NOT NULL;

-- Carry the existing rows across. Every one of them is a document field row,
-- so its scope and title come from its document.
UPDATE search_index si
   SET workspace_id     = d.workspace_id,
       project_id       = d.project_id,
       team_id          = d.team_id,
       title_or_preview = d.title
  FROM document d
 WHERE d.id = si.document_id;

ALTER TABLE search_index ALTER COLUMN workspace_id SET NOT NULL;

ALTER TABLE search_index ADD CONSTRAINT search_index_pkey PRIMARY KEY (id);

-- What the old primary key actually guaranteed: one row per document field.
-- Kept as a unique constraint so the write path's delete-then-insert cannot
-- double up.
ALTER TABLE search_index
    ADD CONSTRAINT search_index_document_field_key UNIQUE (document_id, field_id);

ALTER TABLE search_index
    ADD CONSTRAINT search_index_exactly_one_source
        CHECK (num_nonnulls(ticket_id, document_id, comment_id) = 1),

    -- A document source is always a field within that document, which is
    -- LAM-26's "aspect_field" source type. A field_id without a document has
    -- nothing to be a field of.
    ADD CONSTRAINT search_index_document_rows_carry_a_field
        CHECK ((document_id IS NOT NULL) = (field_id IS NOT NULL));

-- Generated rather than an expression index: selectable for ranking and
-- highlighting, and Postgres owns it so it cannot drift from content.
ALTER TABLE search_index
    ADD COLUMN search tsvector
        GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;

CREATE INDEX search_index_search_idx ON search_index USING gin (search);

-- Scoping. workspace_id leads because every query starts there; the two
-- narrower scopes get their own partial indexes and serve their CASCADEs.
CREATE INDEX search_index_workspace_idx ON search_index (workspace_id);
CREATE INDEX search_index_project_idx   ON search_index (project_id) WHERE project_id IS NOT NULL;
CREATE INDEX search_index_team_idx      ON search_index (team_id)    WHERE team_id    IS NOT NULL;

-- The two source arms the unique constraint does not already index. It leads
-- with document_id, so the document arm and its CASCADE are covered.
CREATE INDEX search_index_ticket_idx  ON search_index (ticket_id)  WHERE ticket_id  IS NOT NULL;
CREATE INDEX search_index_comment_idx ON search_index (comment_id) WHERE comment_id IS NOT NULL;

COMMENT ON COLUMN search_index.field_id IS
    'Still text, not uuid, so no foreign key reaches aspect_type_field. Retyping needs body keys validated at the write boundary first - see the why block and LAM-44.';

COMMENT ON COLUMN search_index.search IS
    'Generated from content with the english configuration. Per-language search would need a regconfig column and a different index.';

-- +migrate Down

DROP INDEX search_index_comment_idx;
DROP INDEX search_index_ticket_idx;
DROP INDEX search_index_team_idx;
DROP INDEX search_index_project_idx;
DROP INDEX search_index_workspace_idx;
DROP INDEX search_index_search_idx;

-- Only document rows fit the old shape. Nothing writes the other two arms
-- yet, so this is a no-op today - but it is here so the Down stays correct
-- once something does.
DELETE FROM search_index WHERE document_id IS NULL;

ALTER TABLE search_index
    DROP COLUMN search,
    DROP CONSTRAINT search_index_document_rows_carry_a_field,
    DROP CONSTRAINT search_index_exactly_one_source,
    DROP CONSTRAINT search_index_document_field_key;

-- Same ordering trap in reverse: the new primary key is on id, so it has to
-- be dropped before id itself can go, and document_id cannot regain NOT NULL
-- while anything still depends on the old shape.
ALTER TABLE search_index DROP CONSTRAINT search_index_pkey;

ALTER TABLE search_index ALTER COLUMN document_id SET NOT NULL;
ALTER TABLE search_index ALTER COLUMN field_id    SET NOT NULL;

ALTER TABLE search_index ADD CONSTRAINT search_index_pkey PRIMARY KEY (document_id, field_id);

ALTER TABLE search_index
    DROP COLUMN parent_reference,
    DROP COLUMN title_or_preview,
    DROP COLUMN team_id,
    DROP COLUMN project_id,
    DROP COLUMN workspace_id,
    DROP COLUMN comment_id,
    DROP COLUMN ticket_id,
    DROP COLUMN id;

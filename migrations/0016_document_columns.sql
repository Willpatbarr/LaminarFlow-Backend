-- LAM-23: finishing the document table.
--
-- document was built by 0001 with an id, a jsonb body and timestamps, and
-- 0003 added workspace_id under LAM-4. The hybrid storage model - blob is the
-- source of truth, search_index is derived and rebuildable - was settled in
-- LAM-3 and is not touched here. This migration adds the columns around the
-- blob that were always meant to exist: scope, title, type, and the aspect
-- type reference.
--
--
-- Why every added column is either nullable or defaulted
--
-- Service.Save inserts exactly (workspace_id, body). A NOT NULL column with
-- no default would break every insert the moment this migration applied -
-- which is precisely what 0003 did to Save under LAM-4, and the reason
-- migrations/README.md has an expand-and-contract section at all.
--
-- title and type are NOT NULL DEFAULT, and the defaults are honest rather
-- than papering over the question. '' is a genuinely untitled document, the
-- same call document.body (DEFAULT '{}') and ticket.description (DEFAULT '')
-- already make. 'normal' states a fact about every row that exists today:
-- there is no other kind of document yet, because aspect_type_id has not
-- existed until now. Neither default invents an answer the caller was
-- supposed to give.
--
--
-- Scope is an exclusive arc, and at most one, not exactly one
--
-- LAM-23 says "project_id or team_id" and leaves both settable. setting hit
-- the same question under LAM-16 and answered it with an exclusive arc rather
-- than a polymorphic scope_type/scope_id pair, for the reason that a
-- polymorphic column cannot carry a foreign key. Same answer here.
--
-- The difference from setting is the count. setting requires exactly one
-- scope, because a setting belonging to nothing is meaningless. A document
-- belonging to neither a project nor a team is not meaningless - it is a
-- workspace-level document, which is what every row written before this
-- migration is, and what the ticket means by "optional child". So the CHECK
-- is <= 1, not = 1.
--
-- Both are ON DELETE SET NULL. The ticket names no action, which would leave
-- NO ACTION and make a project undeletable while any document sat in it.
-- CASCADE would be worse: deleting a project would destroy documents that
-- merely happened to be filed under it. SET NULL drops the document back to
-- workspace level, which the CHECK above deliberately permits.
--
--
-- aspect_type_id is RESTRICT, and that reaches further than it looks
--
-- The three options are not close. CASCADE deletes every document of an
-- aspect type when that type is deleted - the same destructive shape LAM-18
-- rejected for ticket.status_id, and worse here because a document is the
-- work itself rather than a label on it. SET NULL would leave type = 'aspect'
-- with no aspect type, which the biconditional CHECK below exists to forbid.
-- RESTRICT is what is left, and it is also right: deleting a document shape
-- that documents are still using should require dealing with the documents.
--
-- The consequence worth stating: aspect_type CASCADEs from team, so deleting
-- a team is now blocked whenever one of its aspect types has documents. That
-- is consistent with document.workspace_id already being RESTRICT - documents
-- have always blocked deletion of the thing above them - but it is a new way
-- for a team delete to fail, and it is asserted in a test rather than left to
-- be discovered.
--
-- No index on aspect_type_id. RESTRICT only has to find one referencing row
-- and can stop there, which is the bounded case migrations/README.md names as
-- the exception to indexing a foreign key, and nothing reads documents by
-- aspect type yet. The two SET NULL columns do get indexes, because SET NULL
-- has to find and rewrite every referencing row. Both are partial: the
-- overwhelming majority of documents will carry neither scope, and there is
-- no reason to index the nulls.
--
--
-- The type CHECK is biconditional, which is stronger than LAM-23 asks
--
-- Step 2 asks that aspect_type_id be set only when type = 'aspect'. Read
-- literally that is one-directional - it forbids an aspect type on a normal
-- document and permits an aspect document with no aspect type. The second
-- case is incoherent: an aspect document is defined by its shape, and one
-- without a shape is a normal document that lies about itself. So the CHECK
-- is an equality of the two conditions.
--
-- type is a closed set for status.category's reason: the value is what every
-- type filter and every renderer branches on, so a typo like 'apsect' would
-- silently drop a document out of all of them rather than failing.

-- +migrate Up

ALTER TABLE document
    ADD COLUMN project_id     uuid REFERENCES project(id)     ON DELETE SET NULL,
    ADD COLUMN team_id        uuid REFERENCES team(id)        ON DELETE SET NULL,
    ADD COLUMN aspect_type_id uuid REFERENCES aspect_type(id) ON DELETE RESTRICT,
    ADD COLUMN title          text NOT NULL DEFAULT '',
    ADD COLUMN type           text NOT NULL DEFAULT 'normal';

ALTER TABLE document
    -- At most one narrowing scope. Neither set is a workspace-level document,
    -- which is what every row written before this migration is.
    ADD CONSTRAINT document_at_most_one_scope
        CHECK (num_nonnulls(project_id, team_id) <= 1),

    -- Closed set, like status.category.
    ADD CONSTRAINT document_type_is_known
        CHECK (type IN ('normal', 'aspect')),

    -- Both directions: an aspect document must have a shape, and a normal
    -- document must not have one.
    ADD CONSTRAINT document_aspect_type_matches_type
        CHECK ((type = 'aspect') = (aspect_type_id IS NOT NULL));

-- Both scopes are SET NULL, so both need an index whether or not a read wants
-- one - SET NULL has to find every referencing row before it can rewrite it.
-- Partial, because most documents carry neither scope.
CREATE INDEX document_project_idx ON document (project_id) WHERE project_id IS NOT NULL;
CREATE INDEX document_team_idx    ON document (team_id)    WHERE team_id    IS NOT NULL;

COMMENT ON COLUMN document.type IS
    'Closed set: normal, aspect. An aspect document carries aspect_type_id; a normal one must not.';

COMMENT ON COLUMN document.aspect_type_id IS
    'RESTRICT: an aspect type still in use by documents cannot be deleted, which also blocks deleting the team above it.';

-- +migrate Down

DROP INDEX document_team_idx;
DROP INDEX document_project_idx;

ALTER TABLE document
    DROP CONSTRAINT document_aspect_type_matches_type,
    DROP CONSTRAINT document_type_is_known,
    DROP CONSTRAINT document_at_most_one_scope,
    DROP COLUMN type,
    DROP COLUMN title,
    DROP COLUMN aspect_type_id,
    DROP COLUMN team_id,
    DROP COLUMN project_id;

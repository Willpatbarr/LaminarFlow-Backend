-- Document folders: a project's documents filed in a tree of folders.
--
-- A folder belongs to exactly one project and may sit inside another folder of the same
-- project (parent_id), so a project's folders are a tree, like a file system. A document
-- sits in at most one folder (document.folder_id); null is the project's top level, which
-- is also where every document written before this migration is.
--
--
-- Only project documents are filed
--
-- A folder needs a project, and a document in a folder must be in that folder's project.
-- Team-wide and workspace-level documents have no folders: there is no project to hang
-- the tree from, and nothing asks for one yet.
--
-- The rule is a composite foreign key, (folder_id, project_id) -> (id, project_id), the
-- shape 0033 used for ticket.milestone_id. It cannot be broken by a write that moves a
-- document to another project and forgets its folder: the statement that does that fails.
--
-- document.project_id is nullable (0016), and a composite key whose columns are partly null
-- is not checked (MATCH SIMPLE). So a second, plain key on folder_id alone is what makes a
-- dangling folder_id impossible: when a project is deleted its folders cascade away and its
-- documents' project_id is set null, in whichever order Postgres fires those, and the plain
-- key still clears folder_id. Both keys only ever clear folder_id, never project_id.
--
--
-- Deleting a folder deletes no documents
--
-- The service re-files a folder's documents and sub-folders into the folder's parent before
-- it deletes the folder, in one transaction (internal/document/folder.go). The schema backs
-- that up rather than relying on it: document.folder_id is SET NULL, so a folder deleted any
-- other way drops its documents to the project's top level instead of taking them with it,
-- and parent_id is NO ACTION, so deleting a folder that still has sub-folders fails instead
-- of silently deleting a subtree. A project delete still works: every folder of the project
-- goes in the one cascade, and NO ACTION is checked at the end of the statement.
--
-- Cycles (a folder moved under its own descendant) are refused by the service, under a
-- per-project lock. A CHECK cannot see further than its own row; the self-parent case is
-- the one it can, so it is here as well.
--
-- No name uniqueness. Two folders called "Specs" side by side are confusing, but documents
-- already allow duplicate titles and a uniqueness rule is easy to add later and hard to take
-- back.

-- +migrate Up

CREATE TABLE document_folder (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid        NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    parent_id  uuid,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- The target of both composite keys below, and the project's folder listing.
    CONSTRAINT document_folder_project_id_key UNIQUE (project_id, id),

    CONSTRAINT document_folder_parent_fkey
        FOREIGN KEY (project_id, parent_id) REFERENCES document_folder (project_id, id),

    CONSTRAINT document_folder_not_own_parent CHECK (parent_id <> id)
);

-- For the NO ACTION check on delete: it has to find a folder's children.
CREATE INDEX document_folder_parent_idx ON document_folder (parent_id) WHERE parent_id IS NOT NULL;

ALTER TABLE document ADD COLUMN folder_id uuid;

ALTER TABLE document
    ADD CONSTRAINT document_folder_fkey
        FOREIGN KEY (folder_id) REFERENCES document_folder (id) ON DELETE SET NULL,
    ADD CONSTRAINT document_folder_project_fkey
        FOREIGN KEY (project_id, folder_id) REFERENCES document_folder (project_id, id)
        ON DELETE SET NULL (folder_id);

-- For the SET NULLs: deleting a folder has to find its documents first. Partial, because
-- most documents sit at a project's top level.
CREATE INDEX document_folder_idx ON document (folder_id) WHERE folder_id IS NOT NULL;

COMMENT ON COLUMN document.folder_id IS
    'The folder the document is filed in, one of its project''s. Null is the project''s top level, and always for a document with no project.';

-- +migrate Down

DROP INDEX document_folder_idx;
ALTER TABLE document
    DROP CONSTRAINT document_folder_project_fkey,
    DROP CONSTRAINT document_folder_fkey,
    DROP COLUMN folder_id;

DROP TABLE document_folder;

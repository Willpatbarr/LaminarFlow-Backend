-- LAM-18: the ticket table.
--
-- Child of project, and the first table that references three parents at
-- once. The three foreign keys deliberately do not share an ON DELETE action,
-- because the three parents mean different things to a ticket:
--
--   project_id           CASCADE   a ticket cannot exist outside a project
--   status_id            SET NULL  a deleted status leaves the ticket statusless
--   assignee_account_id  SET NULL  a deleted account leaves the ticket unassigned
--
-- The two SET NULLs are the load-bearing choices. CASCADE on either would be
-- destructive in a way nobody would ask for: deleting a status would delete
-- every ticket in that column, and deleting an account would delete every
-- ticket that person was assigned. Work outlives both the workflow it sat in
-- and the people who touched it.
--
-- status_id nullable is LAM-17's decision, recorded on LAM-18 - no status is
-- sacred, so deleting one must not be blocked, which means tickets have to be
-- able to hold no status. Every board and filter carries that case forever.
--
-- epic_id and milestone_or_release_id are NOT here. LAM-18 step 3 allows them
-- as bare nullable columns with no FK target, but a uuid column referencing
-- nothing is a column that can hold garbage until its table lands, and there
-- is no ticket for either table yet. Whichever ticket builds epic, milestone
-- and release adds its own column with a real reference at the same time.
--
-- That also settles the "or" in milestone_or_release_id: a milestone and a
-- release are different things, so they become two nullable columns, not one
-- ambiguous one. Deferred with the tables.
--
-- assignee is named assignee_account_id. Every other reference here is
-- <table>_id, and "assignee" alone does not say what it points at.
--
-- description, not body. document.body is jsonb holding a field map keyed by
-- field ID, and search_index derives from it; a ticket's prose is a different
-- thing entirely. Reusing the name across two tables with two types and two
-- meanings is the kind of collision that costs a reader ten minutes. It is
-- NOT NULL DEFAULT '' rather than nullable, following document.body, so
-- nothing has to decide whether null and empty differ.

-- +migrate Up

CREATE TABLE ticket (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id          uuid        NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    status_id           uuid        REFERENCES status(id) ON DELETE SET NULL,
    assignee_account_id uuid        REFERENCES account(id) ON DELETE SET NULL,
    title               text        NOT NULL,
    description         text        NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- The board read: one project's tickets, grouped by status. project_id leads,
-- so this also serves the foreign key.
CREATE INDEX ticket_project_status_idx ON ticket (project_id, status_id);

-- These two exist for the ON DELETE actions, not for a read. A SET NULL has
-- to find every referencing row before it can null it, so without an index
-- here deleting one status or one account scans the whole ticket table. The
-- index on project_id above happens to cover its own CASCADE for the same
-- reason - see "Foreign keys are not indexed for you" in the README.
CREATE INDEX ticket_status_id_idx ON ticket (status_id);
CREATE INDEX ticket_assignee_account_id_idx ON ticket (assignee_account_id);

-- +migrate Down

DROP TABLE ticket;

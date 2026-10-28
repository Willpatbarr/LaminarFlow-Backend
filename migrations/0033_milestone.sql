-- LAM-47: the milestone table, ticket.milestone_id and epic.milestone_id.
--
-- The other optional structural level between project and ticket. Built the
-- way 0022_epic.sql builds epic: master spec 3.2 makes every intermediate
-- level independently optional, so a milestone sits directly in a project and
-- project_id is NOT NULL.
--
--
-- Milestone only, no release (LAM-82)
--
-- Master spec Open Question 1 asked whether Release and Milestone both exist,
-- are aliases, or only one ships. LAM-82 decided only one ships, and it is
-- Milestone: a release is just a milestone. There is no release table and no
-- kind column. A Release entity can be added later if a real need appears;
-- nothing here forecloses it.
--
-- That also retires the two-columns plan in 0011_ticket.sql. With one entity
-- there is no "or" to settle and no pair of columns: ticket gets a single
-- nullable milestone_id.
--
--
-- The cross-project hole, closed the same way as epic
--
-- A plain ticket.milestone_id REFERENCES milestone(id) would file an Apollo
-- ticket under a Borealis milestone. So both referencing tables reach
-- milestone through a composite foreign key carrying the shared project_id:
--
--   ticket (milestone_id, project_id) -> milestone (id, project_id)
--   epic   (milestone_id, project_id) -> milestone (id, project_id)
--
-- ticket and epic both already carry a NOT NULL project_id, so the referencing
-- side costs nothing new. The README's test for this pattern is "is a
-- mismatch ever legitimate", and for a ticket or an epic under another
-- project's milestone it never is.
--
-- The UNIQUE is (project_id, id) for the reason 0022_epic.sql gives: one
-- btree serves the foreign key target, the CASCADE from project, and listing
-- one project's milestones, because project_id leads. Asserted in a test.
--
--
-- Deletes
--
-- project -> milestone is CASCADE, matching epic and sprint.
--
-- milestone -> ticket and milestone -> epic are SET NULL (milestone_id), not
-- CASCADE: work outlives the planning scope it was filed under, LAM-18's
-- reason. The column list is load-bearing, as in 0022: project_id is NOT NULL
-- on both referencing tables, so a bare SET NULL on the composite key would
-- raise 23502 and make any milestone with work in it undeletable.
--
-- ON UPDATE is NO ACTION: moving a ticket or an epic to another project while
-- it sits in a milestone is rejected rather than silently allowed.
--
-- epic.milestone_id is the nullable column 0022_epic.sql left for this
-- migration. An epic with no milestone is the common case and stays legal;
-- the composite key is MATCH SIMPLE, so a null milestone_id switches the
-- check off.
--
-- description is text NOT NULL DEFAULT '', following epic and ticket. No
-- UNIQUE (project_id, name): epic, project, sprint and status all decline
-- one, and LAM-47 does not ask for it. No dates: the ticket does not ask for
-- them and a column can be added later; a milestone is just a named scope.

-- +migrate Up

CREATE TABLE milestone (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid        NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- Three jobs in one btree, as on epic. project_id leads deliberately.
    CONSTRAINT milestone_project_id_key UNIQUE (project_id, id)
);

ALTER TABLE ticket ADD COLUMN milestone_id uuid;

ALTER TABLE ticket ADD CONSTRAINT ticket_milestone_fkey
    FOREIGN KEY (milestone_id, project_id) REFERENCES milestone (id, project_id)
    ON DELETE SET NULL (milestone_id);

ALTER TABLE epic ADD COLUMN milestone_id uuid;

ALTER TABLE epic ADD CONSTRAINT epic_milestone_fkey
    FOREIGN KEY (milestone_id, project_id) REFERENCES milestone (id, project_id)
    ON DELETE SET NULL (milestone_id);

-- For the SET NULLs, not for a read: deleting a milestone has to find every
-- ticket and epic referencing it first.
CREATE INDEX ticket_milestone_id_idx ON ticket (milestone_id);
CREATE INDEX epic_milestone_id_idx ON epic (milestone_id);

-- +migrate Down

ALTER TABLE epic DROP CONSTRAINT epic_milestone_fkey;
DROP INDEX epic_milestone_id_idx;
ALTER TABLE epic DROP COLUMN milestone_id;

ALTER TABLE ticket DROP CONSTRAINT ticket_milestone_fkey;
DROP INDEX ticket_milestone_id_idx;
ALTER TABLE ticket DROP COLUMN milestone_id;

DROP TABLE milestone;

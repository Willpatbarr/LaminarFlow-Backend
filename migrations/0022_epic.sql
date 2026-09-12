-- LAM-46: the epic table, and ticket.epic_id.
--
-- The optional structural level between project and ticket. Master spec 3.2
-- makes both intermediate levels independently optional, so an epic sits
-- directly in a project and project_id is NOT NULL. Whatever builds
-- milestone/release (LAM-47) adds a nullable epic.milestone_id then.
--
-- This table was missed by E-LAM-0003 the first time round. 0011_ticket.sql
-- deliberately omitted epic_id rather than ship a uuid pointing at nothing,
-- and named the ticket that would add it with a real reference. This is it.
--
--
-- The cross-project hole, closed
--
-- ticket is a child of project. epic is a child of project. A plain
-- ticket.epic_id REFERENCES epic(id) would happily file an Apollo ticket
-- under a Borealis epic, and the symptom surfaces later as an epic quietly
-- listing work from another project.
--
-- Same hole ticket_sprint had, same fix: reach the parent through a composite
-- foreign key carrying the shared project_id, so the ticket's project and the
-- epic's project are one value by construction.
--
--   (epic_id, project_id) -> epic (id, project_id)
--
-- LAM-42 rejected this same pattern for team_member and that rejection does
-- not carry here. It turned on outside collaborators being a case the product
-- wants, so forcing the two memberships to agree would have banned a feature.
-- There is no equivalent case for a ticket sitting in another project's epic;
-- it is not a feature, it is a leak. The README's test for this pattern is
-- not "do both sides share a parent" but "is a mismatch ever legitimate", and
-- here it never is.
--
-- Cheaper here than in 0013. ticket already carries UNIQUE (id, project_id)
-- from that migration, so the referencing side costs nothing new.
--
--
-- Why the UNIQUE is (project_id, id) and not (id, project_id)
--
-- Postgres matches a foreign key to its target index by column SET, not by
-- column order, so REFERENCES epic (id, project_id) is satisfied by a unique
-- constraint declared either way round. That leaves the order free to do a
-- second job, and (project_id, id) leads with project_id - which is the index
-- the CASCADE from project needs, and the one "list a project's epics" wants.
--
-- So one btree serves three purposes and epic gets no separate project_id
-- index. That is the "a UNIQUE constraint already indexes the foreign key"
-- case from migrations/README.md, with id standing in for name. 0013 built
-- UNIQUE (id, project_id) plus a separate read index because its read wanted
-- a different leading column; this table's read and its foreign key want the
-- same one, so they share.
--
-- The order is load-bearing and it breaks silently. Flip it to
-- (id, project_id) and the foreign key still resolves, the migration still
-- applies, every constraint test still passes - and deleting one project
-- starts scanning every epic in the instance. Asserted in a test for exactly
-- that reason, the same assertion 0005_team.sql earned.
--
--
-- Deletes, in three directions
--
-- project -> epic is CASCADE. An epic for a project that no longer exists is
-- not an epic, matching sprint.
--
-- epic -> ticket is SET NULL, not CASCADE. Deleting an epic must not delete
-- the work filed under it - the same call LAM-18 made for status_id and
-- assignee_account_id. Work outlives the organisational structure it sat in.
--
-- The column list on that SET NULL is not decoration. A composite foreign key
-- nulls every one of its referencing columns by default, and project_id is
-- NOT NULL - so a bare ON DELETE SET NULL would raise 23502 on every attempt
-- and leave any epic with tickets in it permanently undeletable. The
-- column-list form nulls only the column that should be nulled. It needs
-- Postgres 15; the Pi and CI both run 17.
--
-- ON UPDATE is NO ACTION on both sides, with the same consequence 0013 has:
-- moving a ticket to another project while it sits in an epic is rejected
-- rather than silently allowed. ON UPDATE CASCADE would not help - it would
-- drag project_id to the new project and the epic side would fail anyway,
-- because the epic did not move. Whatever moves a ticket between projects has
-- to take it out of its epic first. Asserted, so it is a decision rather than
-- something a future reader meets in production.
--
--
-- A ticket with no epic is the common case and stays legal. The composite
-- foreign key is MATCH SIMPLE, so a null epic_id switches the check off
-- entirely and the non-null project_id beside it is never consulted. That is
-- the whole reason the optional level can be optional.
--
-- description is text NOT NULL DEFAULT '', following ticket.description, so
-- nothing downstream has to decide whether null and empty mean different
-- things.
--
-- No UNIQUE (project_id, name). project, sprint and status all decline one
-- and LAM-46 does not ask for it; two epics may share a name.

-- +migrate Up

CREATE TABLE epic (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid        NOT NULL REFERENCES project(id) ON DELETE CASCADE,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- Three jobs in one btree: the target the composite foreign key below
    -- requires, the index the CASCADE from project needs, and the index for
    -- listing one project's epics. project_id leads deliberately.
    CONSTRAINT epic_project_id_key UNIQUE (project_id, id)
);

ALTER TABLE ticket ADD COLUMN epic_id uuid;

-- project_id is already on ticket and already NOT NULL, so this reaches epic
-- through the pair rather than through epic_id alone. One project_id, two
-- roles: the ticket's own parent and the epic's, forced equal.
ALTER TABLE ticket ADD CONSTRAINT ticket_epic_fkey
    FOREIGN KEY (epic_id, project_id) REFERENCES epic (id, project_id)
    ON DELETE SET NULL (epic_id);

-- For the SET NULL, not for a read. Postgres has to find every ticket
-- referencing an epic before it can null them, so without this, deleting one
-- epic scans every ticket in the instance. Same reason ticket_status_id_idx
-- and ticket_assignee_account_id_idx exist in 0011.
CREATE INDEX ticket_epic_id_idx ON ticket (epic_id);

-- +migrate Down

-- The constraint before the column it lives on, and both before the table
-- they point at.
ALTER TABLE ticket DROP CONSTRAINT ticket_epic_fkey;
DROP INDEX ticket_epic_id_idx;
ALTER TABLE ticket DROP COLUMN epic_id;

DROP TABLE epic;

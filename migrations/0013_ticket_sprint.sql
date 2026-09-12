-- LAM-20: ticket_sprint, the join table between ticket and sprint.
--
-- A sprint associates tickets from anywhere in its project rather than owning
-- them structurally. That is the whole point of the table: it is what lets a
-- ticket belong to an epic structurally and to a sprint temporally at the same
-- time, without either parent having to know about the other.
--
-- Composite PK on (ticket_id, sprint_id). No surrogate id - the pair IS the
-- fact, and a uuid PK would only invite two rows saying the same thing.
--
--
-- The cross-project hole, and what it costs to close it
--
-- A ticket belongs to a project. A sprint belongs to a project. Two plain
-- foreign keys here would happily pair a ticket in Apollo with a sprint in
-- Borealis, and nothing in the database would object. That is not a feature
-- anyone would ask for; it is a data-integrity leak that would surface as a
-- board quietly showing a ticket from somewhere else.
--
-- So this table carries a redundant project_id and reaches both parents
-- through composite foreign keys:
--
--   (ticket_id, project_id) -> ticket (id, project_id)
--   (sprint_id, project_id) -> sprint (id, project_id)
--
-- One project_id, two references, so the ticket's project and the sprint's
-- project are the same value by construction. There is no state in which they
-- disagree, and no application code has to remember to check.
--
-- The price is paid on the parents. Postgres will only point a foreign key at
-- columns carrying a unique constraint, so ticket and sprint each need
-- UNIQUE (id, project_id) purely as a target. Both are logically redundant -
-- id is already the primary key, so adding project_id cannot make the pair any
-- more unique - and each builds a second btree on a table that did not ask for
-- one. That is the real cost of this pattern and it is worth it here: the
-- alternative is an invariant that lives only in application code and holds
-- only as long as every future writer remembers it.
--
-- This same pattern was proposed for team_member under LAM-42 and rejected,
-- deliberately. That rejection does not carry here. It turned on outside
-- collaborators being real - a person on a team without being a member of the
-- workspace above it is a case the product wants. There is no equivalent case
-- for a ticket from another project turning up in your sprint.
--
-- project_id has no foreign key of its own to project. It does not need one:
-- it can only hold a value that some ticket and some sprint both carry, and
-- both of those already reference project. A third constraint would be
-- checking a fact the first two have already established.
--
--
-- Deletes, and the one thing this blocks
--
-- CASCADE on both sides. An association row naming a deleted ticket or a
-- deleted sprint is not a record of anything, it is a dangling pair - there is
-- no product decision hiding in this one. Deleting a project reaches here the
-- same way, through ticket and sprint, which both CASCADE from project.
--
-- Neither foreign key declares ON UPDATE, so both are NO ACTION, and that has
-- a consequence worth naming. Moving a ticket to another project while it sits
-- in one of the old project's sprints is now rejected rather than silently
-- allowed. That is the correct behaviour and the only coherent one available:
-- ON UPDATE CASCADE would drag this row's project_id to the new project, and
-- then the sprint-side reference would fail anyway, because the sprint did not
-- move. Whatever moves a ticket between projects has to take it out of its
-- sprints first. Asserted in a test so it is a decision, not a surprise.
--
--
-- One index, not two
--
-- LAM-20 step 2 asks for indexes on both columns. Only one is built, because
-- the primary key already is the other one: a composite PK on
-- (ticket_id, sprint_id) builds a btree led by ticket_id, which serves both
-- lookups by ticket and the ticket-side CASCADE. A separate single-column
-- index on ticket_id would be a second copy of information the PK already
-- carries - see the "foreign keys are not indexed for you" section of
-- migrations/README.md, which is exactly this case.
--
-- The other direction genuinely has nothing, so it is built: sprint_id leads,
-- and ticket_id follows to make "which tickets are in this sprint" - the read
-- a sprint board does on every load - answerable from the index alone, without
-- touching the heap. It serves the sprint-side CASCADE at the same time.
--
--
-- created_at and updated_at are both here, following every other table in this
-- schema. updated_at is honest dead weight on this one: every column except
-- the timestamps is part of the primary key, so a row cannot be edited, only
-- deleted and reinserted, and the value will never move off its default.
-- Consistency across ten tables is worth more than saving eight bytes on the
-- one table that cannot use it. created_at earns its place - it records when a
-- ticket entered the sprint, which is what sprint scope-change history is.
--
-- No UNIQUE beyond the primary key, and no CHECK. LAM-20 asks for neither, and
-- the pair being unique is already all this table claims.

-- +migrate Up

-- Foreign key targets for the composite references below. Both are redundant
-- as uniqueness claims - id alone is already unique on each table - and exist
-- only because Postgres requires a unique constraint on referenced columns.
ALTER TABLE ticket ADD CONSTRAINT ticket_id_project_key UNIQUE (id, project_id);
ALTER TABLE sprint ADD CONSTRAINT sprint_id_project_key UNIQUE (id, project_id);

CREATE TABLE ticket_sprint (
    ticket_id  uuid        NOT NULL,
    sprint_id  uuid        NOT NULL,
    project_id uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (ticket_id, sprint_id),

    -- One project_id, reached through both parents, so a ticket and a sprint
    -- in different projects cannot be paired at all.
    CONSTRAINT ticket_sprint_ticket_fkey FOREIGN KEY (ticket_id, project_id)
        REFERENCES ticket (id, project_id) ON DELETE CASCADE,
    CONSTRAINT ticket_sprint_sprint_fkey FOREIGN KEY (sprint_id, project_id)
        REFERENCES sprint (id, project_id) ON DELETE CASCADE
);

-- The sprint board read: every ticket in one sprint. ticket_id follows so the
-- answer comes out of the index without a heap lookup. Also the index the
-- sprint-side CASCADE uses. The ticket-side direction needs nothing here - the
-- primary key is already a btree led by ticket_id.
CREATE INDEX ticket_sprint_sprint_idx ON ticket_sprint (sprint_id, ticket_id);

COMMENT ON COLUMN ticket_sprint.project_id IS
    'Denormalised from both parents. Carries no reference of its own; the two composite foreign keys force it to equal the ticket''s project and the sprint''s project at once.';

-- +migrate Down

DROP TABLE ticket_sprint;

ALTER TABLE sprint DROP CONSTRAINT sprint_id_project_key;
ALTER TABLE ticket DROP CONSTRAINT ticket_id_project_key;

-- LAM-49: ticket_label, the join table between ticket and label.
--
-- Many labels on a ticket, many tickets on a label. Composite PK on
-- (ticket_id, label_id); no surrogate id, because the pair IS the fact and a
-- uuid PK would only invite two rows saying the same thing. Same shape as
-- ticket_sprint.
--
--
-- Tickets only, and what happens if documents ever want labels
--
-- Master spec 3.1 speaks about tickets. Labelling documents is plausible and
-- nobody has asked for it, so this table names ticket_id outright rather than
-- carrying a target_type/target_id pair. If documents get labels later the
-- answer is a second join table, document_label, not a polymorphic column -
-- the fourth time this epic has reached that conclusion, after LAM-23,
-- LAM-24, LAM-26 and LAM-42. A polymorphic target here would also make the
-- cross-team constraint below unexpressible, since a composite foreign key
-- cannot point at two different parents.
--
--
-- The cross-team hole, and why closing it costs three foreign keys
--
-- A label belongs to a team. A ticket belongs to a project, which belongs to
-- a team. Two plain foreign keys would let an Apollo ticket carry a label
-- belonging to a different team entirely, and the symptom surfaces as a
-- filter returning work from somewhere else - or a label surviving in the UI
-- after the team that owns it is gone.
--
-- This is the pattern from migrations/README.md, "A join table between two
-- children of the same parent", but it is NOT a copy of 0013. There, ticket
-- and sprint are both direct children of project, so one denormalised
-- project_id and two composite foreign keys closed it. Here the shared parent
-- is team and it sits two levels above ticket, which carries no team_id of
-- its own. The chain has to be walked a hop at a time:
--
--   (ticket_id, project_id) -> ticket  (id, project_id)
--   (project_id, team_id)   -> project (id, team_id)
--   (label_id,   team_id)   -> label   (id, team_id)
--
-- Read bottom to top: the label's team is this row's team_id, the project's
-- team is the same team_id, and the ticket's project is the same project_id.
-- So the ticket's team and the label's team are one value by construction and
-- there is no state in which they disagree. Two denormalised columns and
-- three references, rather than one and two.
--
-- The alternative considered and rejected was denormalising team_id onto
-- ticket itself, which would have made this a two-hop copy of 0013. It would
-- also have put a column on ticket that no ticket-level read wants, that
-- LAM-18 deliberately did not include, and that every ticket writer would
-- then have to keep in step with its project. The cost belongs on the table
-- that needs the invariant, not on the parent.
--
-- LAM-42 rejected this whole pattern for team_member and that rejection does
-- not carry here either. It turned on outside collaborators being a case the
-- product wants. A ticket wearing another team's label is not a case anyone
-- wants; it is a leak. The README's test is "is a mismatch ever legitimate",
-- and here it never is.
--
-- Two parents pay for it. project and label each need UNIQUE (id, team_id)
-- purely as a foreign key target - both logically redundant, since id is
-- already unique on each. ticket pays nothing: it already carries
-- UNIQUE (id, project_id) from 0013.
--
-- Note the contrast with 0022_epic.sql, which chose UNIQUE (project_id, id)
-- so the target constraint doubled as the project_id index. Neither target
-- here can do that. project already has project_team_id_idx and label's own
-- UNIQUE (team_id, name) already leads with team_id, so in both cases the
-- index job is done and the column order carries no second duty. Written
-- (id, team_id) to match 0013's naming rather than to earn anything.
--
-- team_id and project_id carry no foreign keys of their own to team or
-- project. They cannot hold a value the three references above have not
-- already established.
--
--
-- Deletes, and what must not be blocked
--
-- CASCADE on all three. Deleting a label removes it from every ticket rather
-- than blocking the delete - the same call master spec 3.4 makes for statuses
-- and LAM-17 made for status, and for the same reason: a label is user-owned
-- and deletable, and work must not pin it in place. Deleting a ticket removes
-- its labels, which are not a record of anything without it. Deleting a
-- project or a team reaches here through both legs at once.
--
-- Crucially the reverse does not hold: removing a label from a ticket must
-- never touch the ticket. Nothing here can do that - CASCADE only ever
-- deletes rows of this table - but it is asserted anyway, because it is the
-- destructive mistake this table is one keystroke away from.
--
-- ON UPDATE is NO ACTION throughout, with two consequences, both asserted:
-- a labelled ticket cannot be moved to another project, and a project whose
-- tickets carry labels cannot be moved to another team. The second is the
-- more surprising one and it is also the correct one - a project changing
-- teams leaves every label on its tickets pointing at a team that no longer
-- owns that work. Whatever moves either has to unlabel first.
--
--
-- Two indexes, both directions
--
-- The primary key is a btree led by ticket_id, so "which labels are on this
-- ticket" - the read every ticket render does - and the ticket-side CASCADE
-- are both already served. A separate index on ticket_id would be a second
-- copy of what the PK holds.
--
-- The other direction has nothing, and it is the direction the feature exists
-- for: "every ticket labelled bug" is the filter master spec 3.1 is
-- describing. Built as (label_id, ticket_id) so that read is answered from
-- the index alone without touching the heap, and so the label-side CASCADE
-- has an index too.
--
-- project_id and team_id get no index of their own. Neither has a foreign key
-- to the table it names, so neither has a CASCADE to serve, and no read
-- filters this table by project or team without going through one of the two
-- above.
--
-- created_at and updated_at follow every other table here. As in 0013,
-- updated_at is honest dead weight - every non-timestamp column is part of
-- the primary key or forced by a reference, so a row is deleted and
-- reinserted rather than edited. Consistency across twenty tables outweighs
-- eight bytes. created_at records when the label went on, which is the only
-- history this table can offer.

-- +migrate Up

-- Foreign key targets for the composite references below. Both are redundant
-- as uniqueness claims - id alone is already unique on each table - and exist
-- only because Postgres requires a unique constraint on referenced columns.
-- ticket needs nothing: 0013 already added ticket_id_project_key.
ALTER TABLE project ADD CONSTRAINT project_id_team_key UNIQUE (id, team_id);
ALTER TABLE label   ADD CONSTRAINT label_id_team_key   UNIQUE (id, team_id);

CREATE TABLE ticket_label (
    ticket_id  uuid        NOT NULL,
    label_id   uuid        NOT NULL,
    project_id uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (ticket_id, label_id),

    -- The chain. One project_id ties this row to its ticket and to the
    -- project whose team it names; one team_id ties that project to the same
    -- team the label belongs to. A ticket cannot wear another team's label.
    CONSTRAINT ticket_label_ticket_fkey FOREIGN KEY (ticket_id, project_id)
        REFERENCES ticket (id, project_id) ON DELETE CASCADE,
    CONSTRAINT ticket_label_project_fkey FOREIGN KEY (project_id, team_id)
        REFERENCES project (id, team_id) ON DELETE CASCADE,
    CONSTRAINT ticket_label_label_fkey FOREIGN KEY (label_id, team_id)
        REFERENCES label (id, team_id) ON DELETE CASCADE
);

-- The read this table exists for: every ticket carrying one label.
-- ticket_id follows so the answer comes out of the index without a heap
-- lookup, and the label-side CASCADE uses it too. The ticket-side direction
-- needs nothing - the primary key is already a btree led by ticket_id.
CREATE INDEX ticket_label_label_idx ON ticket_label (label_id, ticket_id);

COMMENT ON COLUMN ticket_label.team_id IS
    'Denormalised. Carries no reference of its own; the project and label composite foreign keys force it to equal the project''s team and the label''s team at once.';
COMMENT ON COLUMN ticket_label.project_id IS
    'Denormalised from the ticket. Carries no reference of its own; it is the middle hop tying the ticket to the team that owns the label.';

-- +migrate Down

DROP TABLE ticket_label;

ALTER TABLE label   DROP CONSTRAINT label_id_team_key;
ALTER TABLE project DROP CONSTRAINT project_id_team_key;

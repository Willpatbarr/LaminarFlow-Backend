-- LAM-24: the comment table, with review requests folded into it.
--
-- A review request is not a parallel system. It is the same row with extra
-- columns populated - review_status NULL is a plain comment, non-null makes
-- the same row a review request. That is the decorator the schema notes ask
-- for, and it is why comment_reviewer (LAM-25) hangs off comment rather than
-- off a review_request table that does not exist.
--
--
-- The target is an exclusive arc, not target_type/target_id
--
-- This is the one place this migration departs from LAM-24's field list, and
-- it is deliberate. The ticket names a polymorphic pair and links a reference
-- for the pattern. This epic has answered that question twice already and
-- both times the other way:
--
--   LAM-16, setting: "scope is an exclusive arc, not scope_type/scope_id"
--   LAM-42, membership: two join tables, because a polymorphic scope_id
--                       cannot carry a foreign key
--
-- The cost is not stylistic. Postgres cannot point one foreign key at two
-- tables, so target_type/target_id means no reference on the thing being
-- commented on at all - and therefore no ON DELETE anything. Delete a ticket
-- and every comment on it stays behind pointing at a uuid that resolves to
-- nothing. No error, no cascade, and no join that can find them again. A
-- misconfigured setting is a bad row; an orphaned comment is lost writing.
--
-- Two nullable columns with real foreign keys cost a CHECK and buy CASCADE.
-- The price is the one setting already pays and already documented: a third
-- target type costs a migration adding a column and widening the constraint.
-- That is a known, bounded, one-off cost, paid by whoever adds the third
-- target - as against an unbounded cost paid forever by everything that
-- touches comments.
--
-- CASCADE on both. A comment on a deleted ticket is not a record of anything.
--
--
-- author_id is nullable and SET NULL
--
-- LAM-24 lists author_id among the plain fields, which reads as NOT NULL. It
-- is not, and the reason is LAM-18's: work outlives the people who touched
-- it. The three options for deleting an account that has commented are to
-- block the delete (RESTRICT), destroy the discussion (CASCADE), or keep the
-- discussion and lose the attribution (SET NULL). Only the third is a
-- product anyone would want, and it requires the column to be nullable.
--
-- Everything reading comments must therefore render an authorless comment.
-- That is the same obligation ticket.assignee_account_id already created.
--
--
-- field_id anchors a comment to one Aspect field, and is SET NULL for the
-- same reason: deleting a field should not delete the conversation about it,
-- only unanchor it. It is also gated to document targets - the ticket says
-- "if the target is a multi-field Aspect document", and unenforced a comment
-- on a ticket could carry a field anchor that means nothing.
--
-- No constraint ties field_id to the target document's own aspect type. It
-- could be done with the composite-FK pattern from 0013, but document's
-- aspect_type_id is nullable (a normal document has none) and a composite
-- foreign key through a nullable column does not constrain the rows where it
-- is null - which is every plain document. The check that a field belongs to
-- the document's type belongs at the application boundary, alongside the
-- document.body key check already recorded on LAM-26.
--
--
-- The region columns
--
-- LAM-24 gives two nullable integers and no relationship between them. Three
-- absences there are incoherence rather than product policy, so they are
-- constrained here and flagged the way sprint.end_date >= start_date was:
--
--   * half a region - a start with no end - cannot be highlighted
--   * a region ending before it starts is not a region
--   * a negative offset is not a position in any content
--
-- region_end = region_start is deliberately allowed. That is a caret, a
-- zero-length insertion point, which is a real thing to comment on. Using >
-- here would look equally correct and silently ban it - the same trap
-- 0012_sprint.sql documents for one-day sprints.
--
--
-- review_status is a closed set for status.category's reason: reporting and
-- the review queue branch on it, so 'aproved' would drop a review request out
-- of every list rather than failing. NULL stays outside the set and is the
-- default state, because a plain comment is the common case.
--
-- body is NOT NULL with no default, unlike ticket.description. A ticket
-- legitimately has no description yet; a comment with nothing in it is not a
-- comment. LAM-24 asks for no minimum length and none is invented.
--
-- No parent_comment_id. LAM-24 does not ask for threading and inventing a
-- self-reference now would guess at a UI nobody has designed.

-- +migrate Up

CREATE TABLE comment (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Exactly one target, with a real foreign key each, instead of a
    -- target_type/target_id pair that could carry neither.
    ticket_id     uuid        REFERENCES ticket(id)            ON DELETE CASCADE,
    document_id   uuid        REFERENCES document(id)          ON DELETE CASCADE,

    field_id      uuid        REFERENCES aspect_type_field(id) ON DELETE SET NULL,
    region_start  integer,
    region_end    integer,
    author_id     uuid        REFERENCES account(id)           ON DELETE SET NULL,
    body          text        NOT NULL,
    review_status text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT comment_exactly_one_target
        CHECK (num_nonnulls(ticket_id, document_id) = 1),

    -- A field anchor only means something on a document.
    CONSTRAINT comment_field_needs_a_document
        CHECK (field_id IS NULL OR document_id IS NOT NULL),

    -- Both ends or neither.
    CONSTRAINT comment_region_is_whole
        CHECK (num_nonnulls(region_start, region_end) <> 1),

    -- >= not >, so a zero-length caret stays legal.
    CONSTRAINT comment_region_ends_after_it_starts
        CHECK (region_end >= region_start),

    CONSTRAINT comment_region_starts_at_or_after_zero
        CHECK (region_start >= 0),

    -- NULL is a plain comment and stays outside the set.
    CONSTRAINT comment_review_status_is_known
        CHECK (review_status IN ('pending', 'approved', 'changes_requested'))
);

-- The read LAM-24 step 2 asks for - every comment on one target - split in
-- two because the target is. Each also serves its own CASCADE. Partial,
-- because exactly one of the two is null on every row.
CREATE INDEX comment_ticket_idx   ON comment (ticket_id)   WHERE ticket_id   IS NOT NULL;
CREATE INDEX comment_document_idx ON comment (document_id) WHERE document_id IS NOT NULL;

-- "Show my pending review requests". Partial because the overwhelming
-- majority of rows are plain comments with a null status, and indexing those
-- would be indexing precisely what the query excludes.
CREATE INDEX comment_review_status_idx ON comment (review_status)
    WHERE review_status IS NOT NULL;

-- Both SET NULL, so both need an index whether or not a read wants one.
CREATE INDEX comment_field_idx  ON comment (field_id)  WHERE field_id  IS NOT NULL;
CREATE INDEX comment_author_idx ON comment (author_id) WHERE author_id IS NOT NULL;

COMMENT ON COLUMN comment.review_status IS
    'NULL is a plain comment. Non-null makes the same row a review request - the decorator, not a separate table.';

COMMENT ON COLUMN comment.author_id IS
    'Nullable and SET NULL: deleting an account keeps the discussion and loses the attribution. Readers must render an authorless comment.';

-- +migrate Down

DROP TABLE comment;

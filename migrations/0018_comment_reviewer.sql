-- LAM-25: comment_reviewer, the join table between comment and account.
--
-- Populated only when the parent comment is acting as a review request.
-- Multiple reviewers per request, each independently able to approve or
-- request changes - so the review state that varies per reviewer is not here
-- yet; comment.review_status is the request's overall state, and LAM-25 lists
-- no per-reviewer verdict column. Not invented here.
--
-- Composite PK on (comment_id, account_id). The same person cannot be asked
-- to review the same comment twice.
--
--
-- Step 3, and why it is not application-level
--
-- LAM-25 step 3 asks for "application-level validation that rows are only
-- inserted here when the parent comment has a non-null review_status". That
-- validation is in the database instead, because it turns out to be
-- expressible - and a reviewer attached to a plain comment is incoherence
-- rather than product policy, the same line this epic drew for
-- sprint.end_date >= start_date and status.category's closed set.
--
-- The mechanism, and why it is shaped the way it is:
--
-- comment gains a stored generated column, is_review_request, which is just
-- (review_status IS NOT NULL). Being generated, it cannot drift from the
-- column it summarises. comment then carries UNIQUE (id, is_review_request)
-- purely as a foreign key target, the same redundant-constraint cost 0013
-- paid on ticket and sprint.
--
-- comment_reviewer carries its own is_review_request, pinned true by a CHECK
-- and never written by a caller, and reaches the parent through a composite
-- foreign key on (comment_id, is_review_request). Because this side is
-- always true, the only comment rows it can match are the ones whose
-- review_status is non-null. A plain comment fails with a foreign key
-- violation.
--
-- The obvious alternative - a composite foreign key on review_status itself -
-- looks simpler and is wrong. review_status moves: pending to approved is the
-- entire point of a review. A foreign key on that value would have to cascade
-- on every transition, and with ON UPDATE NO ACTION it would block them
-- outright. Generating a boolean that only changes at the NULL boundary means
-- ordinary review transitions cost nothing.
--
-- Both halves of the enforcement are load-bearing and neither replaces the
-- other. The foreign key stops a row pointing at a comment that is not a
-- review request. The CHECK stops a caller writing is_review_request = false
-- explicitly, which would otherwise match a plain comment's (id, false) and
-- walk straight through the foreign key.
--
-- There is deliberately no ON UPDATE clause, so it is NO ACTION. The only
-- change the referenced key can undergo is true to false, which is exactly
-- the change that must be refused - so a cascade would never usefully fire,
-- and declaring one would claim the child follows the parent when it never
-- can. NO ACTION says what actually happens: the update is refused while a
-- reviewer row still references the old value.
--
-- Verified against the real database before this was written, all four cases:
-- a reviewer on a review request is accepted; a reviewer on a plain comment
-- is rejected with 23503; pending -> approved with a reviewer attached
-- succeeds and keeps the reviewer; and demoting a review request back to a
-- plain comment while reviewers are attached is rejected with 23514.
--
-- That last one is a new behaviour worth naming rather than discovering. A
-- review request with reviewers cannot be turned back into a plain comment -
-- the reviewers have to be unassigned first. The alternative is silently
-- dropping them, which is worse.
--
--
-- Deletes
--
-- ON DELETE CASCADE on both sides, and they mean different things. Deleting
-- the comment deletes the request, so its reviewer rows go with it. Deleting
-- an account removes that person's assignments - unlike comment.author_id,
-- which is SET NULL because authorship is a historical fact worth keeping
-- even without an account, an assignment to a person who no longer exists is
-- not a record of anything. It is also not optional: account_id is half the
-- primary key, so it cannot be nulled.
--
--
-- The index LAM-25 step 2 asks for is genuinely needed, and the ticket says
-- why: the composite PK leads with comment_id, so it serves "who is reviewing
-- this comment" but not "which review requests are assigned to me". That
-- second direction has nothing without this index. It is widened to
-- (account_id, comment_id) so the review-queue read comes back out of the
-- index without touching the heap, and it serves the account-side CASCADE at
-- the same time. The comment direction needs no index of its own - the
-- primary key is already a btree led by comment_id, and the composite foreign
-- key's leading column is the same.

-- +migrate Up

-- Generated, so it cannot drift from review_status, and stable across
-- pending -> approved -> changes_requested.
ALTER TABLE comment
    ADD COLUMN is_review_request boolean
        GENERATED ALWAYS AS (review_status IS NOT NULL) STORED;

-- Redundant as a uniqueness claim - id is already the primary key - and
-- present only because Postgres requires a unique constraint on the columns a
-- foreign key references.
ALTER TABLE comment
    ADD CONSTRAINT comment_id_review_key UNIQUE (id, is_review_request);

CREATE TABLE comment_reviewer (
    comment_id        uuid        NOT NULL,
    account_id        uuid        NOT NULL REFERENCES account(id) ON DELETE CASCADE,

    -- Always true, never written by a caller. It exists to give the composite
    -- foreign key below something that can only match a review request.
    is_review_request boolean     NOT NULL DEFAULT true,

    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (comment_id, account_id),

    CONSTRAINT comment_reviewer_parent_is_a_review_request
        CHECK (is_review_request),

    CONSTRAINT comment_reviewer_comment_fkey
        FOREIGN KEY (comment_id, is_review_request)
        REFERENCES comment (id, is_review_request)
        ON DELETE CASCADE
);

-- "Review requests assigned to me". account_id leads because the primary key
-- already covers the other direction; comment_id trails so the answer comes
-- out of the index alone. Also the index the account-side CASCADE uses.
CREATE INDEX comment_reviewer_account_idx
    ON comment_reviewer (account_id, comment_id);

COMMENT ON COLUMN comment_reviewer.is_review_request IS
    'Always true. Pinned so the composite foreign key can only match a comment whose review_status is non-null.';

COMMENT ON COLUMN comment.is_review_request IS
    'Generated from review_status. Exists so comment_reviewer can reference "is a review request" without referencing the mutable status itself.';

-- +migrate Down

DROP TABLE comment_reviewer;

ALTER TABLE comment DROP CONSTRAINT comment_id_review_key;
ALTER TABLE comment DROP COLUMN is_review_request;

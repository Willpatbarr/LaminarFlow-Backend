-- LAM-132: comment.edited_at, and the review decision (who, when, note).
--
-- edited_at. comment.updated_at is "the last time the row changed". For a plain comment that
-- is a body edit, but for a review request a status change bumps it too (deliberately: the
-- realtime event's `at` and the stale-approval write read it), so a client that showed
-- "(edited)" whenever updated_at moved past created_at marked a review note edited the moment
-- a reviewer approved it. updated_at keeps its meaning. edited_at is new and narrower: NULL
-- until the body is changed by PUT /comments/{id}, then that time. Only that endpoint writes it.
--
-- Backfill. A plain comment (review_status IS NULL) can only have been moved by a body edit,
-- so edited_at = updated_at where that is later than created_at. A review request cannot be
-- told apart (a status change moved updated_at the same way), and the commoner cause is the
-- status change, so its edited_at stays NULL: an old review note edited before this change
-- loses its mark rather than every decided one wrongly keeping it.
--
-- decided_by, decided_at, decision_note: the last time a reviewer approved or requested
-- changes, who, and the optional note they left. They are set together by that status
-- change and are kept (not cleared) when the request goes back to pending, by a reviewer or
-- because the text changed, so a client can still say who approved before. A request that was
-- never decided has all three NULL. decided_by is SET NULL when the account is deleted, like
-- author_id. Backfill: a request already approved or changes_requested gets decided_at =
-- updated_at; decided_by stays NULL (unknown) and a client renders "a reviewer".

-- +migrate Up

ALTER TABLE comment
    ADD COLUMN edited_at     timestamptz,
    ADD COLUMN decided_by    uuid REFERENCES account (id) ON DELETE SET NULL,
    ADD COLUMN decided_at    timestamptz,
    ADD COLUMN decision_note text;

UPDATE comment SET edited_at = updated_at
 WHERE review_status IS NULL AND updated_at > created_at;

UPDATE comment SET decided_at = updated_at
 WHERE review_status IN ('approved', 'changes_requested');

ALTER TABLE comment ADD CONSTRAINT comment_decision_is_review
    CHECK (review_status IS NOT NULL OR (decided_by IS NULL AND decided_at IS NULL AND decision_note IS NULL));

COMMENT ON COLUMN comment.edited_at IS
    'When the body was last edited (LAM-132). NULL if never. updated_at also moves on a review status change; this does not.';
COMMENT ON COLUMN comment.decided_by IS
    'Who last approved or requested changes on a review request (LAM-132). Kept while pending again; NULL if undecided, unknown or the account is gone.';
COMMENT ON COLUMN comment.decided_at IS
    'When a reviewer last approved or requested changes (LAM-132).';
COMMENT ON COLUMN comment.decision_note IS
    'The optional note that came with that decision (LAM-132).';

-- +migrate Down

ALTER TABLE comment DROP CONSTRAINT comment_decision_is_review;
ALTER TABLE comment
    DROP COLUMN decision_note,
    DROP COLUMN decided_at,
    DROP COLUMN decided_by,
    DROP COLUMN edited_at;

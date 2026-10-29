-- LAM-122: comment.stale_reason, why an approved review request went back to pending.
--
-- An approval is about the text the reviewer saw. When a document write changes that text
-- materially (docs/review-requests.md, "Stale approvals") the request is sent back to
-- 'pending'. Nothing recorded why: a request that is pending because it was never looked at
-- and one that is pending because it was approved and then invalidated looked the same.
--
-- Nullable. Set only together with review_status = 'pending', and cleared by every status
-- change that leaves pending (approved or changes_requested), which is what the CHECK
-- enforces; plain comments have no review_status and so never a reason. The values are free
-- text for a reader to show; the only one written today is 'anchored text changed'.

-- +migrate Up

ALTER TABLE comment ADD COLUMN stale_reason text;

ALTER TABLE comment ADD CONSTRAINT comment_stale_reason_needs_pending
    CHECK (stale_reason IS NULL OR coalesce(review_status = 'pending', false));

COMMENT ON COLUMN comment.stale_reason IS
    'Why an approved review request went back to pending (LAM-122). NULL otherwise; set only while review_status is pending.';

-- +migrate Down

ALTER TABLE comment DROP CONSTRAINT comment_stale_reason_needs_pending;
ALTER TABLE comment DROP COLUMN stale_reason;

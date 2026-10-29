-- LAM-113: comment.anchor_text, the text a review request was anchored to.
--
-- A review request points at a range of a document field by character offsets
-- (region_start, region_end; docs/review-requests.md says exactly what they count). Offsets
-- say where the request was made, not what it was about: the moment the field is edited they
-- may point at different words. Storing the words themselves, as they stood when the request
-- was made, is what lets a later reader (LAM-122: approvals going stale when the reviewed
-- text materially changes; a UI re-anchoring or orphaning a request) compare the text now at
-- the range against the text that was reviewed, and, when the offsets have drifted, search
-- the field for the quote. ADR 6 anticipated this: the anchor lives in the request's own row,
-- since the Markdown string carries no marker.
--
-- Nullable, with no CHECK tying it to review_status or to the region. NULL means "no text was
-- recorded": every plain comment, and every row that predates this column. A review request
-- over a caret (region_start = region_end) records the empty string, which is a real value
-- and different from NULL. Plain comments anchored to a field (POST /comments) do not record
-- it; only the review-request write path does.
--
-- Not a hash: the text is cheap to keep and a hash of it could not be searched for, shown in
-- a request list, or compared by anything but equality. A reader that wants a hash computes
-- one.

-- +migrate Up

ALTER TABLE comment ADD COLUMN anchor_text text;

COMMENT ON COLUMN comment.anchor_text IS
    'For a review request: the text of the field between region_start and region_end (characters, not bytes) when it was made. NULL when none was recorded. See LAM-113 and docs/review-requests.md.';

-- +migrate Down

ALTER TABLE comment DROP COLUMN anchor_text;

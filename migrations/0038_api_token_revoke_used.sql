-- LAM-91: revocation and last-used tracking for api_token.
--
-- revoked_at is set instead of deleting the row, so a revoked token keeps
-- answering "you were revoked" in the audit trail and its prefix stays taken. A
-- presented revoked token is rejected in Go beside the expiry check, never in
-- SQL: a WHERE predicate would skip the bcrypt comparison and hand back the
-- timing difference dummyHash exists to remove.
--
-- last_used_at is what a token management screen shows. Written best effort and
-- at most once a minute per token, so it is a hint rather than a ledger.
--
-- Both are nullable: null revoked_at means active, null last_used_at means never
-- used. No index on either - revocation is checked on a row already fetched by
-- prefix, and the list read is by account_id, which 0008 indexes.

-- +migrate Up

ALTER TABLE api_token
    ADD COLUMN revoked_at   timestamptz,
    ADD COLUMN last_used_at timestamptz;

-- +migrate Down

ALTER TABLE api_token
    DROP COLUMN last_used_at,
    DROP COLUMN revoked_at;

-- LAM-56: the session table.
--
-- Child of account, and the second credential in this schema. api_token
-- (0008) authenticates scripts and agents; this authenticates a person in a
-- browser. Deleting an account deletes both - a credential outliving the
-- account it authenticates is an open door, so CASCADE here is a security
-- property rather than a convenience, exactly as 0008 argues.
--
--
-- Why a table at all, rather than a signed cookie
--
-- Settled on LAM-56. A signed cookie carries its own claims and needs no
-- storage, which is genuinely simpler - and it cannot be revoked. "Log out"
-- would only delete the cookie from the browser doing the logging out; a copy
-- taken beforehand, or one sitting on a laptop the owner no longer has, keeps
-- working until it expires and nothing can stop it.
--
-- Three things decided it:
--
--   1. Logout has to actually revoke. Deleting a row does; forgetting a cookie
--      does not.
--   2. The lookup is not a new cost. Every authenticated request already hits
--      Postgres to do its actual work, and this is one primary-key-indexed
--      read next to that - trivial beside the bcrypt api_token already pays.
--   3. A signed cookie needs a signing secret: a value in internal/config, a
--      deployment story for where it lives, and a rotation path that logs
--      everyone out when it runs. A table needs no secret at all. The
--      stateless option is simpler only until key management is counted.
--
-- The deciding argument was consistency. API Design notes section 5 already
-- accepted a per-request Postgres lookup for api_token *because revocation
-- matters*. Stateless sessions would leave the same product answering "can I
-- revoke this credential?" two different ways depending on which one you
-- hold, and the gap only surfaces on the day it matters.
--
--
-- token_hash is SHA-256, and deliberately not bcrypt
--
-- This looks like it contradicts 0008, which requires a slow hash on
-- token_hash. It does not, and the difference is worth stating because the
-- next reader will notice.
--
-- A slow hash defends a *guessable* secret. api_token's secret is
-- high-entropy, but 0008 chose bcrypt anyway and pays it once per request -
-- roughly 50 to 100 ms on the Pi. A browser session is read on every page
-- load and every background poll, so the same choice here would put that cost
-- in front of the whole UI.
--
-- The session id is 32 random bytes. There is no dictionary to run against it
-- and no rainbow table that helps, so a fast hash gives the same protection
-- that matters here: a stolen database dump yields no usable cookies, because
-- SHA-256 is not reversible. Slow hashing buys nothing more against an input
-- nobody can guess.
--
-- Hashing at all is the point. Storing the raw id would mean a database leak
-- hands over every live session directly.
--
--
-- expires_at is NOT NULL, unlike api_token.expires_at
--
-- 0008 made expiry nullable because a CI token that never expires is a real
-- use. A browser session that never expires is not - it is an account takeover
-- waiting on one borrowed laptop. Every row here has an end.
--
-- Sliding expiry is the application's job, not this table's. The column is a
-- deadline; whatever extends it decides how often, and internal/auth only
-- writes when more than a day has passed, so an active user is not one UPDATE
-- per request.

-- +migrate Up

CREATE TABLE session (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid        NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    token_hash  text        NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- The hot path. Every authenticated request looks a row up by this value,
    -- and the UNIQUE constraint is what indexes it - see "Foreign keys are not
    -- indexed for you" in the README for why no separate index follows.
    UNIQUE (token_hash)
);

-- The index LAM-56 needs on account_id: it serves the CASCADE above, and it
-- is also the read behind "sign out everywhere" and any future session list.
-- The UNIQUE indexes token_hash and the PK indexes id, so nothing else covers
-- it.
CREATE INDEX session_account_id_idx ON session (account_id);

-- Expired rows are rejected at read time by internal/auth, so no query ever
-- searches on expires_at alone and no index here would be used. A sweeper
-- deleting old rows would want one; there is no sweeper yet, and adding the
-- index before the thing that reads it is inventing structure nobody asked
-- for.

COMMENT ON COLUMN session.token_hash IS
    'A SHA-256 hash of the session id held in the cookie. Never the id itself. See this migration''s why block for why this is not bcrypt.';

COMMENT ON COLUMN session.expires_at IS
    'Hard deadline, always set. internal/auth extends it on activity rather than on every request.';

-- +migrate Down

-- DROP TABLE takes the index, the unique constraint, and the comments.
DROP TABLE session;

-- LAM-15: the api_token table.
--
-- Child of account. Deleting an account deletes its tokens - a token that
-- outlives the account it authenticates is an open door, so CASCADE here is a
-- security property rather than a convenience.
--
-- The token is stored split, the way GitHub stores its own. A presented
-- credential is prefix plus secret; the server splits it, looks the row up by
-- token_prefix, and then compares the secret against token_hash.
--
--   token_prefix  non-secret, unique, indexed. This is the lookup key.
--   token_hash    a slow salted hash (bcrypt/argon2) of the secret half.
--
-- LAM-15 asks for the token to be hashed "consistent with the
-- account.password_hash pattern" AND for an index on the hash for fast
-- lookup. Those two cannot both hold: bcrypt salts every hash, so the same
-- token hashes differently every time and no index can find it - a lookup
-- would have to scan every row and bcrypt-compare each one, getting slower
-- with every token issued. Splitting the token is what makes both true at
-- once. The prefix carries the lookup, the hash carries the secret, and
-- exactly one bcrypt comparison happens per request.
--
-- scopes defaults to the empty array, not to a wildcard. A token created
-- without scopes can do nothing, which is the direction an auth default
-- should fail in.
--
-- expires_at and label are not in LAM-15's field list. expires_at is nullable
-- and null means never expires, which is today's behaviour, so the column
-- costs nothing until something sets it. label exists because a token
-- management screen has to show "CI deploy key" rather than a hash prefix,
-- and adding it now avoids a backfill against rows nobody can name later.

-- +migrate Up

CREATE TABLE api_token (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   uuid        NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    label        text        NOT NULL,
    token_prefix text        NOT NULL,
    token_hash   text        NOT NULL,
    scopes       text[]      NOT NULL DEFAULT '{}',
    expires_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- The hot path. Every API-token request looks a row up by this value, and
    -- the UNIQUE constraint is what indexes it - see "Foreign keys are not
    -- indexed for you" in the README for why no separate index follows.
    UNIQUE (token_prefix)
);

-- The index LAM-15 step 4 asks for on account_id. Needed on its own here:
-- the UNIQUE above indexes token_prefix, and the PK indexes id, so nothing
-- else would serve "list this account's tokens".
CREATE INDEX api_token_account_id_idx ON api_token (account_id);

-- No GIN index on scopes yet. Scope checks happen against a row already
-- fetched by prefix, so there is no scopes query to serve. Add one with the
-- ticket that introduces a "which tokens grant scope X" read.
--
-- No index on expires_at either, for the same reason: expiry is checked on a
-- row already in hand, not searched for.

COMMENT ON COLUMN api_token.token_prefix IS
    'Non-secret lookup key, the first segment of the presented token. Indexed.';

COMMENT ON COLUMN api_token.token_hash IS
    'A bcrypt or argon2 hash of the secret half of the token. Never plaintext, and never the whole token.';

-- +migrate Down

-- DROP TABLE takes the index, the unique constraint, and the comments.
DROP TABLE api_token;

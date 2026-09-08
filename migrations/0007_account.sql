-- LAM-14: the account table.
--
-- The first table in this schema with no parent. An account is not owned by a
-- workspace or a team: it may belong to several workspaces, and it may sit on
-- a team without being a member of that team's workspace at all. Those
-- relationships are many-to-many and live in workspace_member and team_member
-- (LAM-42), not in columns here - a column holds one value, and an account
-- needs many.
--
-- Email is unique case-insensitively, through a unique index on lower(email)
-- rather than a plain UNIQUE constraint. Someone who signed up as
-- Will@example.com and later types will@example.com is the same person; a
-- plain constraint would let both rows exist and then fail the login.
--
-- password_hash is NOT NULL. LAM-14 allows "or auth identifier" for a future
-- external-auth path, but making the column nullable now would express an
-- option nothing uses, and dropping NOT NULL later is a one-line migration
-- that breaks no existing code. Tightening is the expensive direction, so the
-- tight version ships first.
--
-- display_name is NOT NULL, matching every other name in the schema. LAM-14
-- does not say either way.

-- +migrate Up

CREATE TABLE account (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    password_hash text        NOT NULL,
    display_name  text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Unique on the folded address, not the raw one. This is also the index a
-- login reads, since a login looks up lower(email) too.
CREATE UNIQUE INDEX account_email_lower_key ON account (lower(email));

-- LAM-14 step 3 asks that password_hash never hold plaintext. A migration
-- cannot enforce what the application writes, so it records the contract
-- where anyone opening psql will read it - the same reason 0004 exists.
COMMENT ON COLUMN account.password_hash IS
    'A bcrypt or argon2 hash of the password. Never plaintext.';

-- +migrate Down

-- DROP TABLE takes the index and the column comment with it.
DROP TABLE account;

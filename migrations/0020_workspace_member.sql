-- LAM-42: workspace_member, the join table between account and workspace.
--
-- account carries no workspace column, because a column holds one value and
-- an account needs many. This is the first half of that; team_member in 0021
-- is the second.
--
-- Two tables rather than one polymorphic membership(account_id, scope_type,
-- scope_id). The deciding reason, recorded on LAM-42 when it was written: a
-- polymorphic scope_id cannot carry a foreign key, so Postgres could not
-- guarantee that deleting a workspace removes its membership rows. It also
-- leaves room for the two levels to diverge on extra fields later, which is
-- preferred over columns that are null for half the rows.
--
-- This is the third time this epic has declined a polymorphic reference -
-- setting's scope under LAM-16, comment's target under LAM-24, search_index's
-- source under LAM-26 - and the reason has been identical every time.
--
--
-- The composite-FK pattern is deliberately NOT used here
--
-- Worth stating plainly, because 0013 and 0018 both reach for it and a reader
-- moving through these files in order will expect it again.
--
-- Nothing enforces that a team member is also a workspace member. The
-- composite-FK pattern could enforce it, and LAM-42 considered and rejected
-- it: an outside collaborator may sit on a single team with no workspace-wide
-- access, and that is a case the product wants. Forcing the two levels to
-- agree would ban a feature rather than prevent a mistake.
--
-- That is the same test migrations/README.md now states for the pattern - the
-- question is not whether two tables share a parent, but whether a mismatch
-- is ever legitimate. For ticket_sprint it never was. Here it routinely is.
-- The two levels are independent, not nested.
--
--
-- role is the permissions role, the same vocabulary at both levels. text with
-- a CHECK rather than a Postgres ENUM, per LAM-42 step 3 and matching
-- status.category: adding a value later is an ALTER TABLE rather than an
-- ALTER TYPE, and the enum type would have to be shared by two tables or
-- duplicated.
--
-- No default. LAM-42 does not name one, and a membership row created without
-- saying what the person can do is a question the caller is supposed to
-- answer - the distinction migrations/README.md draws between a default that
-- is a property of the column and one that invents an answer.
--
-- An organisational role - job title and similar - is deferred until there is
-- a demonstrated need, and would be its own table rather than more values
-- here.
--
--
-- Nothing requires a workspace to have an owner, or to have any members at
-- all. LAM-42 asks for neither. A workspace with no owner is recoverable by
-- an instance administrator and a constraint here could not express "at least
-- one" without a trigger or a deferred check across rows. Asserted as an
-- absence so adding it later is deliberate.
--
--
-- The index LAM-42 step 5 asks to be decided: account_id gets one.
--
-- The composite primary key is a btree led by workspace_id, so it answers
-- "who is in this workspace" and serves the workspace-side CASCADE. It does
-- nothing for "which workspaces does this account belong to", which is the
-- read every session does on login, and nothing for the account-side CASCADE.
-- So the index is real rather than redundant - the case
-- migrations/README.md's foreign-key section calls out as genuinely needing
-- one.
--
-- Widened to (account_id, workspace_id) so that read comes back out of the
-- index without touching the heap.

-- +migrate Up

CREATE TABLE workspace_member (
    workspace_id uuid        NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    account_id   uuid        NOT NULL REFERENCES account(id)   ON DELETE CASCADE,
    role         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (workspace_id, account_id),

    -- Closed set, like status.category. A typo is a member with no effective
    -- permissions rather than an error at the point it is written.
    CONSTRAINT workspace_member_role_is_known
        CHECK (role IN ('owner', 'admin', 'member'))
);

-- "Which workspaces does this account belong to" - the login read. The
-- primary key leads with workspace_id and cannot serve it. workspace_id
-- trails so the answer comes out of the index alone, and this also serves the
-- account-side CASCADE.
CREATE INDEX workspace_member_account_idx
    ON workspace_member (account_id, workspace_id);

COMMENT ON TABLE workspace_member IS
    'Workspace membership and permissions role. Independent of team_member: a team member need not be a workspace member.';

-- +migrate Down

DROP TABLE workspace_member;

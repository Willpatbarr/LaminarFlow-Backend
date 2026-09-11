-- LAM-42: team_member, the join table between account and team.
--
-- The second half of LAM-42. Identical in shape to workspace_member in 0020,
-- and kept as its own file for the reason LAM-42 gives for keeping the two
-- tables separate in the first place: the levels are expected to diverge on
-- extra fields, and a shared file would invite changing both when only one
-- was meant.
--
-- Read 0020's why block first - the reasoning for two tables over a
-- polymorphic membership, for text plus CHECK over an ENUM, for no default on
-- role, and for the account_id index all applies here unchanged.
--
--
-- The one thing specific to this table
--
-- A team member need not be a workspace member, and nothing here enforces
-- otherwise. This is the deliberate case LAM-42 was written around: an
-- outside collaborator sits on a single team with no workspace-wide access.
--
-- The composite-FK pattern used in 0013 and 0018 could force the two to
-- agree, and was considered and rejected for exactly this reason. It would
-- ban a feature rather than prevent a mistake. There is a test asserting the
-- outside collaborator can exist, so restoring the constraint later has to
-- argue with it.
--
-- The reverse also holds and is also asserted: a workspace member may belong
-- to no team at all. The two levels are independent, not nested.

-- +migrate Up

CREATE TABLE team_member (
    team_id    uuid        NOT NULL REFERENCES team(id)    ON DELETE CASCADE,
    account_id uuid        NOT NULL REFERENCES account(id) ON DELETE CASCADE,
    role       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (team_id, account_id),

    CONSTRAINT team_member_role_is_known
        CHECK (role IN ('owner', 'admin', 'member'))
);

-- "Which teams does this account belong to". Same shape and same reasoning as
-- workspace_member_account_idx: the primary key leads with team_id and cannot
-- serve this direction, and this also serves the account-side CASCADE.
CREATE INDEX team_member_account_idx
    ON team_member (account_id, team_id);

COMMENT ON TABLE team_member IS
    'Team membership and permissions role. Independent of workspace_member: an outside collaborator may sit on a team without belonging to its workspace.';

-- +migrate Down

DROP TABLE team_member;

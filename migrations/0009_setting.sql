-- LAM-16: the setting table.
--
-- One table for both flavours of setting - workspace-level (instance-wide
-- config) and team-level (default statuses, Kanban columns) - as the schema
-- notes prefer, rather than columns jammed onto the workspace and team rows.
--
-- The scope is an exclusive arc: two nullable foreign keys and a CHECK that
-- exactly one is set. This is the single-table shape LAM-16 proposes, without
-- the cost of scope_type/scope_id. A polymorphic scope_id cannot carry a
-- foreign key, so deleting a workspace would silently leave its settings
-- behind and nothing would stop a row pointing at a scope that never existed.
-- Here both columns are real references and both cascade.
--
-- The trade accepted: a third scope later - project-level settings, say -
-- needs a migration adding a column and widening the CHECK, where
-- scope_type would have taken a new string value. That is the cheaper
-- direction to be wrong in, because it fails as a migration rather than as
-- orphaned rows nobody notices.
--
-- Named setting, singular, not settings. Every table here is singular -
-- document, workspace, team, project, account, api_token - and one plural
-- among them reads as a mistake. LAM-16's field list says "settings"; this is
-- the finalization step 1 asks for.
--
-- Uniqueness is two partial indexes, not one UNIQUE (workspace_id, team_id,
-- key). Postgres treats NULLs as distinct in a unique constraint, so that
-- constraint would allow two identical team-scoped rows - their workspace_id
-- NULLs never compare equal. The partial indexes also serve the read this
-- table exists for: every setting for one scope.

-- +migrate Up

CREATE TABLE setting (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        REFERENCES workspace(id) ON DELETE CASCADE,
    team_id      uuid        REFERENCES team(id) ON DELETE CASCADE,
    key          text        NOT NULL,
    value        jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- Exactly one scope. Zero would be a setting belonging to nothing; two
    -- would be a setting whose owner is ambiguous.
    CONSTRAINT setting_exactly_one_scope
        CHECK (num_nonnulls(workspace_id, team_id) = 1)
);

-- No CHECK on jsonb_typeof, unlike document.body in 0001. A setting value is
-- legitimately a scalar, an array, or an object: true, 30, or a list of
-- Kanban column names are all valid settings, so constraining the shape here
-- would be constraining the product.

CREATE UNIQUE INDEX setting_workspace_key_idx
    ON setting (workspace_id, key) WHERE workspace_id IS NOT NULL;

CREATE UNIQUE INDEX setting_team_key_idx
    ON setting (team_id, key) WHERE team_id IS NOT NULL;

COMMENT ON TABLE setting IS
    'Key-value settings scoped to exactly one workspace or one team. Scope is an exclusive arc, not a polymorphic reference.';

-- +migrate Down

-- DROP TABLE takes both partial indexes, the check constraint, and the
-- comment with it.
DROP TABLE setting;

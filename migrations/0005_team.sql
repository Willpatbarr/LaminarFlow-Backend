-- LAM-12: the team table.
--
-- Child of workspace. Parent of project, status (workflows are configured per
-- team) and aspect_type (aspect types are team-scoped) once those land.
--
-- CASCADE, not RESTRICT: a workspace owns its teams, and an orphaned team
-- should not exist. Note the asymmetry with document.workspace_id, which is
-- RESTRICT - deleting a workspace that still holds documents fails, while
-- deleting one that holds only teams succeeds. Documents are data and are
-- protected; teams are structure and are owned.

-- +migrate Up

CREATE TABLE team (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid        NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- Team names are unique within a workspace, not globally: two workspaces
    -- may each have a team called "Platform".
    UNIQUE (workspace_id, name)
);

-- No separate index on workspace_id. The UNIQUE constraint above indexes
-- (workspace_id, name), and a btree index serves lookups on its leading
-- column, so a single-column index would be redundant. Postgres does not
-- index a foreign key column on its own, so without that constraint there
-- would be no index here at all.

-- +migrate Down

DROP TABLE team;

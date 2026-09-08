-- LAM-13: the project table.
--
-- Child of team. Parent of ticket and sprint once those land, and the target
-- of document.project_id if LAM-23 resolves its project-or-team question that
-- way.
--
-- CASCADE, not RESTRICT: LAM-13 says a project should not be orphaned, and a
-- team owns its projects the same way a workspace owns its teams. Matches
-- team.workspace_id, so a workspace deletion now cascades two levels down.
--
-- No UNIQUE (team_id, name), deliberately. LAM-13 asks for a plain index on
-- team_id and says nothing about uniqueness, unlike LAM-12, which asked for
-- (workspace_id, name). Two projects in one team may therefore share a name.
-- That is the ticket as written rather than an oversight - if project names
-- should be unique within a team, that is a change to LAM-13, and the index
-- below becomes redundant for the reason the README gives under "Foreign keys
-- are not indexed for you".

-- +migrate Up

CREATE TABLE project (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    uuid        NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The index LAM-13 step 2 asks for. It is load-bearing here in a way it was
-- not on team: nothing else indexes team_id, so without it every lookup of a
-- team's projects scans the whole table.
CREATE INDEX project_team_id_idx ON project (team_id);

-- +migrate Down

-- DROP TABLE takes the index with it.
DROP TABLE project;

-- LAM-21: the aspect_type table.
--
-- An aspect type is the shape of a document: "Class", "Service", "Data
-- Store". The fields that shape carries live in aspect_type_field (LAM-22),
-- and document.aspect_type_id points here - but that column does not exist
-- yet. document was built by 0001 with nothing but a jsonb body, and LAM-23
-- owns auditing it, so the reference lands there rather than being bolted on
-- from this side.
--
-- Child of team, per master spec decision 3, and CASCADE like status: a
-- document shape belongs to the team whose documents use it. Team-scoped
-- rather than workspace-scoped is the balance the spec strikes - consistency
-- within a team, without forcing every team in a workspace into identical
-- schemas. It also puts aspect_type in exactly the same position as status,
-- which is the other thing a team configures for itself.
--
-- The table is three columns and the timestamps, because that is what LAM-21
-- lists. No position column, though status has one and a type picker will
-- want an order eventually; LAM-21 does not ask for it and inventing it now
-- means guessing at a UI that does not exist. No description either.
--
-- No is_system, is_default, or any other flag marking the starter types as
-- special - and this one is not an omission for lack of a request, it is the
-- ticket's own words. Class, Interface/Protocol, Service, Data Store and
-- Design Pattern are "just ordinary rows a team can seed, not privileged
-- system types". A boolean saying otherwise would contradict the design. For
-- the same reason there is no CHECK on name: the starter set is a suggestion,
-- not a closed set, and a team naming its own type "Saga" is the point.
--
-- No UNIQUE (team_id, name). LAM-21 does not ask for one, and status and
-- project both decline the same constraint. Two aspect types in a team may
-- share a name; documents reference them by id, so a duplicate name is a
-- confusing picker rather than an ambiguity. Stated here so the absence does
-- not read as an oversight, and asserted in the tests so adding uniqueness
-- later has to be deliberate rather than accidental.
--
--
-- What this migration does NOT do, and why
--
-- LAM-21 step 3 asks for a starter set of aspect types seeded for teams. That
-- is not here, and it is not an oversight.
--
-- A migration runs once against a database, not once per team. Seeding here
-- would give the five rows to whatever teams happen to exist the moment it is
-- applied and silently nothing to every team created afterwards - an
-- inconsistency with no error attached to it, which is worse than seeding
-- nobody. The obligation belongs to whatever creates a team, and no ticket
-- owns team creation yet.
--
-- LAM-17 step 3 hit this exact wall with default statuses (To Do / In
-- Progress / Done) and left it the same way, for the same reason. That makes
-- two obligations now waiting on the same missing owner, which is worth
-- knowing when it is finally built: a team is not usable the moment its row
-- exists, and there is a growing list of what has to happen next.

-- +migrate Up

CREATE TABLE aspect_type (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    uuid        NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The index LAM-21 step 2 asks for on team_id, widened to carry name. team_id
-- leads it, so the foreign key lookup and its CASCADE are served exactly as a
-- single-column index would serve them, and the read this table actually gets
-- - one team's aspect types in a picker - comes back sorted for free. Same
-- shape and same reasoning as status_team_position_idx in 0010_status.sql.
CREATE INDEX aspect_type_team_name_idx ON aspect_type (team_id, name);

COMMENT ON TABLE aspect_type IS
    'The shape of a document, scoped to a team. Starter types are ordinary rows a team can rename or delete, not privileged system types.';

-- +migrate Down

DROP TABLE aspect_type;

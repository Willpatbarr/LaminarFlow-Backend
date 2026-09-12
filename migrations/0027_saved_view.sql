-- LAM-50: the saved_view table.
--
-- Master spec 3.7:
--
--   "A configured view can be saved and reused. A saved view includes its
--    relevant layout, filters, grouping, sorting, and visible columns/fields.
--    Saved views should be shareable with a team rather than being limited to
--    temporary personal state."
--
-- Specified in enough detail to build, and never written into the Database
-- Schema notes, which is why E-LAM-0003 had no ticket for it until the audit.
--
-- team_id is NOT NULL because "shareable with a team" makes team the sharing
-- boundary. project_id is nullable: a view may be scoped to one project or
-- span the whole team.
--
--
-- Two tables, not one - and what that costs this file
--
-- Settled across LAM-48 and LAM-50 before either was built. A board owns
-- column configuration; a saved view owns filters, sorting and visible
-- fields. One table with a kind column would leave half its columns null on
-- every row, which is the shape this epic has now rejected five times.
--
-- The price is here: a view with layout = 'board' has to name WHICH board, so
-- board_id is nullable and tied to layout by a biconditional CHECK - a board
-- view must have a board and a list view must not, both directions, following
-- document_aspect_type_matches_type in 0016.
--
--
-- The biconditional forces CASCADE on board_id, and this is the subtle part
--
-- ON DELETE SET NULL is the obvious choice for a nullable reference and it is
-- wrong here. Nulling board_id on a row whose layout is still 'board' leaves
-- the CHECK unsatisfiable, so Postgres rejects the whole delete and the board
-- becomes undeletable the moment one view points at it. The constraint that
-- makes the column meaningful is the same constraint that rules out the
-- gentle delete action.
--
-- So CASCADE: deleting a board deletes the views of it. That is also the
-- honest reading - a board view whose board is gone is not a view of
-- anything, and there is no coherent thing to widen it to. A list view is not
-- what the user saved.
--
-- The alternative, keeping SET NULL and dropping the biconditional, would
-- leave layout = 'board' rows carrying no board and every renderer needing a
-- fallback. That is the null-half-the-columns shape again, one column at a
-- time.
--
--
-- The cross-scope holes, and which ones are closed
--
-- Three references and three chances to disagree.
--
-- project_id vs team_id: a project-scoped view must name a project in its own
-- team. project already carries UNIQUE (id, team_id) from 0024, so this is
-- the composite pattern at its cheapest. Nullable project_id makes it MATCH
-- SIMPLE, so a team-wide view skips the check entirely rather than needing a
-- special case.
--
-- Its ON DELETE needs the column-list form for 0022's reason: a composite
-- foreign key nulls every referencing column by default, and team_id is
-- NOT NULL, so a bare SET NULL would raise 23502 and make any project with a
-- saved view undeletable. ON DELETE SET NULL (project_id) widens the view to
-- the team instead, which is what LAM-50 step 1 asks for.
--
-- board_id vs both: a board view must name a board in its own team, and - if
-- the view is also project-scoped - in that project. Two composite
-- references, not one:
--
--   (board_id, team_id)    -> board (id, team_id)       always applies
--   (board_id, project_id) -> board (id, project_id)    applies when scoped
--
-- The second is MATCH SIMPLE too, so it lapses exactly when project_id is
-- null, which is the case where the view makes no claim about a project and
-- has nothing to contradict. board carries UNIQUE (id, team_id) from 0025;
-- the project-side target is new and added here.
--
-- owner_account_id is NOT tied to anything. account is global rather than
-- team-scoped - workspace_member and team_member are what bind a person to a
-- team, and LAM-42 established that those two levels stay independent
-- precisely so an outside collaborator can exist. Constraining a view's owner
-- to a member here would re-ban the case LAM-42 went out of its way to allow.
--
--
-- Deletes
--
-- team_id CASCADE: a view without its team is nothing, and team is the
-- sharing boundary.
--
-- owner_account_id SET NULL, not CASCADE - a shared team view must survive
-- its author leaving, the same call comment.author_id made under LAM-24.
--
-- That leaves one honest rough edge, named rather than fixed. A PRIVATE view
-- whose owner is deleted becomes a row with is_shared = false and no owner:
-- visible to nobody and owned by nobody. The database cannot fix this itself,
-- because the repair is "flip is_shared" and a foreign key action cannot set
-- a second column. A CHECK (is_shared OR owner_account_id IS NOT NULL) would
-- only convert the leak into an undeletable account. So whatever handles
-- account deletion has to reassign or delete private views first; it is
-- asserted here as a known state so the next person meets it in a test rather
-- than in production.
--
--
-- is_shared boolean rather than a visibility closed set. LAM-50 decision 3
-- offers both and the field list names this one; the closed set reads better
-- but buys nothing while there are exactly two states, and status.category's
-- CHECK exists because reporting needs a closed set, which this does not.
-- Widening to a visibility column later is an ALTER and a backfill.
--
-- config is jsonb, following document.body and setting.value. Filters are an
-- arbitrary tree of field, operator and value, and normalising that is its
-- own epic. CHECK (jsonb_typeof(config) = 'object') copies
-- document_body_is_object from 0001 so nothing writes a scalar or an array
-- that the reader cannot walk. Nothing validates what is INSIDE it - the same
-- trade setting.value makes, and worth stating so it is not mistaken for
-- validation.
--
-- layout is a closed CHECK for status.category's reason: every renderer
-- branches on it, so a typo must fail at write time rather than drop the view
-- out of all of them.
--
-- No UNIQUE (team_id, name). No table in this schema has one except label,
-- which earned it, and two people saving "My work" is not a collision.
--
-- Nothing seeds a default view. A team with none is legal - the same homeless
-- bootstrap obligation LAM-43 owns.

-- +migrate Up

-- Foreign key target for the project-side board reference below. board
-- already carries UNIQUE (id, team_id) from 0025; this is the other axis.
ALTER TABLE board ADD CONSTRAINT board_id_project_key UNIQUE (id, project_id);

CREATE TABLE saved_view (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id          uuid        NOT NULL REFERENCES team(id) ON DELETE CASCADE,
    project_id       uuid,
    owner_account_id uuid        REFERENCES account(id) ON DELETE SET NULL,
    board_id         uuid,
    name             text        NOT NULL,
    layout           text        NOT NULL,
    config           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    is_shared        boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- A project-scoped view names a project in its own team. The column list
    -- is load-bearing: team_id is NOT NULL, so a bare SET NULL would fail
    -- rather than widen the view.
    CONSTRAINT saved_view_project_fkey FOREIGN KEY (project_id, team_id)
        REFERENCES project (id, team_id) ON DELETE SET NULL (project_id),

    -- A board view names a board in its own team, and in its own project when
    -- it claims one. CASCADE on both - see the why block; the biconditional
    -- below rules out anything gentler.
    CONSTRAINT saved_view_board_team_fkey FOREIGN KEY (board_id, team_id)
        REFERENCES board (id, team_id) ON DELETE CASCADE,
    CONSTRAINT saved_view_board_project_fkey FOREIGN KEY (board_id, project_id)
        REFERENCES board (id, project_id) ON DELETE CASCADE,

    -- Closed set, like status.category.
    CONSTRAINT saved_view_layout_is_known
        CHECK (layout IN ('list', 'board')),

    -- Both directions: a board view must name a board, and a list view must
    -- not. Same shape as document_aspect_type_matches_type.
    CONSTRAINT saved_view_board_matches_layout
        CHECK ((layout = 'board') = (board_id IS NOT NULL)),

    -- Structure only. Nothing validates what is inside, the same trade
    -- setting.value makes.
    CONSTRAINT saved_view_config_is_object
        CHECK (jsonb_typeof(config) = 'object')
);

-- The team's view list, and the index the team-side CASCADE uses.
CREATE INDEX saved_view_team_name_idx ON saved_view (team_id, name);

-- project_id is SET NULL and owner_account_id is SET NULL, so both need an
-- index whether or not a read wants one - each has to find every referencing
-- row before it can rewrite it. board_id is CASCADE and needs one for the
-- same reason. All three are partial, because most rows carry none of them:
-- a shared team-wide list view has all three null.
CREATE INDEX saved_view_project_idx ON saved_view (project_id) WHERE project_id IS NOT NULL;
CREATE INDEX saved_view_owner_idx   ON saved_view (owner_account_id) WHERE owner_account_id IS NOT NULL;
CREATE INDEX saved_view_board_idx   ON saved_view (board_id) WHERE board_id IS NOT NULL;

COMMENT ON COLUMN saved_view.config IS
    'jsonb: filters, grouping, sorting, visible fields. Only its shape is constrained - the CHECK proves it is an object, nothing proves what is in it.';
COMMENT ON COLUMN saved_view.board_id IS
    'CASCADE, not SET NULL: saved_view_board_matches_layout makes a null board_id illegal while layout is board, so nulling it would only make the board undeletable.';

-- +migrate Down

DROP TABLE saved_view;

ALTER TABLE board DROP CONSTRAINT board_id_project_key;

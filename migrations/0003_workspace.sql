-- LAM-4 step 1: the workspace table.
--
-- A self-hosted instance only ever populates one row, but the table exists
-- so the same schema supports the future multi-tenant hosted offering.
-- Nothing may assume "there is exactly one workspace" - callers get a
-- workspace ID passed through, never a global.
--
-- document.workspace_id is an interim bridge: the target hierarchy is
-- workspace -> team -> project -> document (see Database Schema notes on
-- E-LAM-0000). When team/project land, this column either becomes derived
-- or is replaced by that chain.
--
-- Temporary raw SQL until E-LAM-0002 lands a migration runner.

CREATE TABLE workspace (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Seed the single workspace a self-hosted instance runs with, so existing
-- documents have something to backfill to.
INSERT INTO workspace (name) VALUES ('Default');

ALTER TABLE document
    ADD COLUMN workspace_id uuid REFERENCES workspace(id) ON DELETE RESTRICT;

UPDATE document
    SET workspace_id = (SELECT id FROM workspace WHERE name = 'Default');

ALTER TABLE document
    ALTER COLUMN workspace_id SET NOT NULL;

-- No index on workspace_id yet. Every document read will be workspace-scoped
-- once there is more than one workspace, but there is exactly one and three
-- documents, so indexing now would be guessing at a query pattern that does
-- not exist. Add it with the ticket that introduces the scoped reads.

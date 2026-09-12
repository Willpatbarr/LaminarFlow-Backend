-- LAM-22: the aspect_type_field table.
--
-- One labelled box in a document of a given aspect type: "Attributes",
-- "Methods", "Responsibilities". Child of aspect_type, CASCADE - a field
-- definition without its type is not a definition of anything.
--
-- The ticket does not name an ON DELETE action, which would leave it at
-- NO ACTION and make an aspect_type undeletable the moment it had a single
-- field. That is not a product rule anyone asked for; it is the default
-- showing through. CASCADE matches aspect_type -> team directly above it.
--
--
-- id is the seam
--
-- This is the part that is invisible from the DDL and matters more than
-- anything else in the file. A field's id is exactly the key it occupies in
-- document.body's jsonb object, and therefore exactly what turns up in
-- search_index.field_id. The whole reason documents key on an id rather than
-- on a label is that a team renaming "Methods" to "Operations" must not
-- orphan every document already written.
--
-- So id is uuid, like every other table here, and the JSON key is that uuid's
-- text form. The alternative - a text slug like 'methods' - would give
-- readable keys and would let search_index.field_id carry a real foreign key
-- today, but it breaks the convention on eleven tables and invites exactly
-- the rename that keying by id exists to prevent.
--
-- The cost is worth stating plainly: search_index.field_id is text and this
-- is uuid, so no foreign key can join them as they stand. Nothing enforces
-- that a key in document.body names a field that exists. Closing that needs
-- either a retype of search_index.field_id or a check at the application
-- boundary, and neither belongs to LAM-22 - search_index is LAM-26's, and
-- document.aspect_type_id does not exist yet at all (LAM-23).
--
--
-- position is NOT unique per aspect type, deliberately, and for the reason
-- 0010_status.sql gives: reordering is then a plain UPDATE, where a
-- UNIQUE (aspect_type_id, position) would collide halfway through a swap
-- unless it were deferrable or renumbered through a temporary value. Two
-- fields may share a position and readers sort by (position, label) so the
-- result stays deterministic.
--
-- No UNIQUE (aspect_type_id, label) either. LAM-22 does not ask, and
-- aspect_type, status and project all decline the same constraint. Two fields
-- may share a label; documents key on id, so a duplicate label is a confusing
-- editor rather than an ambiguity. Asserted, so adding it later is deliberate.
--
-- No type or data-type column. A field is a labelled text box - master spec
-- 5.5 - and document.body already stores whatever JSON value it holds.
-- LAM-22 lists four fields and typing them is a design nobody has done.
--
--
-- LAM-22 step 3 wants the Aspect Type editor's backend, so that adding,
-- reordering or removing a field is a row insert, update or delete against
-- this table and never a document body rewrite. That backend does not exist -
-- there is no aspect code in internal/ at all - and this is a schema epic.
-- The table is shaped to make the row-operation approach the easy one, which
-- is what the migration can contribute; the logic belongs with the API.

-- +migrate Up

CREATE TABLE aspect_type_field (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    aspect_type_id uuid        NOT NULL REFERENCES aspect_type(id) ON DELETE CASCADE,
    label          text        NOT NULL,
    position       integer     NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- The index LAM-22 step 2 asks for on aspect_type_id, widened to carry
-- position. aspect_type_id leads it, so the foreign key lookup and its CASCADE
-- are served exactly as a single-column index would serve them, and the read
-- this table actually gets - one aspect type's fields in render order - comes
-- back sorted for free. Same shape and reasoning as status_team_position_idx
-- and aspect_type_team_name_idx.
CREATE INDEX aspect_type_field_type_position_idx
    ON aspect_type_field (aspect_type_id, position);

COMMENT ON COLUMN aspect_type_field.id IS
    'Also the key this field occupies in document.body, and so the value in search_index.field_id. Stable across label renames - that is why documents key on it and not on label.';

-- +migrate Down

DROP TABLE aspect_type_field;

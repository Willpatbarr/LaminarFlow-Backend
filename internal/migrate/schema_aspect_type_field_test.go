package migrate

import (
	"context"
	"testing"
)

// The constraints 0015_aspect_type_field.sql claims.
//
// The seam this table defines - a field's id is the key it occupies in
// document.body - cannot be asserted from here. internal/document owns the
// only code permitted to touch document and search_index, enforced by
// TestNoSQLOutsideThisPackage, so the end-to-end half lives there instead.
// What this file covers is the table itself.
func TestAspectTypeFieldConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	newAspectType := func(t *testing.T, label string) string {
		t.Helper()

		team := newTeam(t, pool, newWorkspace(t, pool, label), "Platform")

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, 'Class')
             RETURNING id::text`, team,
		).Scan(&id); err != nil {
			t.Fatalf("create aspect type: %v", err)
		}

		return id
	}
	insert := func(aspectTypeID, label string, position int) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position)
             VALUES ($1::uuid, $2, $3)`,
			aspectTypeID, label, position)
		return err
	}
	countFields := func(t *testing.T, aspectTypeID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM aspect_type_field WHERE aspect_type_id = $1::uuid`,
			aspectTypeID,
		).Scan(&n); err != nil {
			t.Fatalf("count fields: %v", err)
		}

		return n
	}

	t.Run("rejects a field on an aspect type that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "Methods", 1)
		wantPgError(t, err, foreignKeyViolation, "an aspect type that does not exist")
	})

	t.Run("rejects a null aspect type, label or position", func(t *testing.T) {
		aspectType := newAspectType(t, "nulls")

		_, err := pool.Exec(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position)
             VALUES (NULL, 'Methods', 1)`)
		wantPgError(t, err, notNullViolation, "a null aspect_type_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position)
             VALUES ($1::uuid, NULL, 1)`, aspectType)
		wantPgError(t, err, notNullViolation, "a null label")

		_, err = pool.Exec(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position)
             VALUES ($1::uuid, 'Methods', NULL)`, aspectType)
		wantPgError(t, err, notNullViolation, "a null position")
	})

	t.Run("accepts a field with a type, a label and a position", func(t *testing.T) {
		aspectType := newAspectType(t, "happy-path")

		if err := insert(aspectType, "Attributes", 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := countFields(t, aspectType); got != 1 {
			t.Errorf("aspect type holds %d fields, want 1", got)
		}
	})

	// The absence that makes reordering a plain UPDATE. A
	// UNIQUE (aspect_type_id, position) would collide halfway through a swap.
	t.Run("allows two fields to share a position", func(t *testing.T) {
		aspectType := newAspectType(t, "shared-position")

		if err := insert(aspectType, "Attributes", 1); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(aspectType, "Methods", 1); err != nil {
			t.Fatalf("second insert at the same position: %v", err)
		}
	})

	// The swap that a unique constraint on position would break. This is the
	// operation LAM-22 step 3 says the editor performs, so the schema has to
	// permit it without a temporary value or a deferred constraint.
	t.Run("allows two fields to swap positions with plain updates", func(t *testing.T) {
		aspectType := newAspectType(t, "swap")

		if err := insert(aspectType, "Attributes", 1); err != nil {
			t.Fatalf("insert first: %v", err)
		}
		if err := insert(aspectType, "Methods", 2); err != nil {
			t.Fatalf("insert second: %v", err)
		}

		// Halfway through, both rows sit at position 2.
		if _, err := pool.Exec(ctx,
			`UPDATE aspect_type_field SET position = 2
              WHERE aspect_type_id = $1::uuid AND label = 'Attributes'`, aspectType,
		); err != nil {
			t.Fatalf("first half of the swap: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE aspect_type_field SET position = 1
              WHERE aspect_type_id = $1::uuid AND label = 'Methods'`, aspectType,
		); err != nil {
			t.Fatalf("second half of the swap: %v", err)
		}
	})

	t.Run("allows one label twice in one aspect type", func(t *testing.T) {
		aspectType := newAspectType(t, "duplicate-label")

		if err := insert(aspectType, "Notes", 1); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(aspectType, "Notes", 2); err != nil {
			t.Fatalf("second insert with the same label: %v", err)
		}
		if got := countFields(t, aspectType); got != 2 {
			t.Errorf("aspect type holds %d fields, want 2", got)
		}
	})

	t.Run("allows the same label under two aspect types", func(t *testing.T) {
		first := newAspectType(t, "two-types-a")
		second := newAspectType(t, "two-types-b")

		if err := insert(first, "Methods", 1); err != nil {
			t.Fatalf("insert for the first type: %v", err)
		}
		if err := insert(second, "Methods", 1); err != nil {
			t.Fatalf("insert the same label for a second type: %v", err)
		}
	})

	// Without CASCADE this is NO ACTION, and an aspect type becomes
	// undeletable the moment it has one field.
	t.Run("deleting an aspect type deletes its fields", func(t *testing.T) {
		aspectType := newAspectType(t, "type-cascade")

		if err := insert(aspectType, "Doomed", 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM aspect_type WHERE id = $1::uuid`, aspectType,
		); err != nil {
			t.Fatalf("delete aspect type: %v", err)
		}

		if got := countFields(t, aspectType); got != 0 {
			t.Errorf("%d fields survived their aspect type, want 0", got)
		}
	})

	// workspace to team to aspect_type to aspect_type_field. Four levels, and
	// no single table's own test covers a break in another's.
	t.Run("deleting a workspace cascades all the way to the field", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "full-chain-field")
		team := newTeam(t, pool, workspace, "Platform")

		var aspectType string
		if err := pool.QueryRow(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, 'Class')
             RETURNING id::text`, team,
		).Scan(&aspectType); err != nil {
			t.Fatalf("create aspect type: %v", err)
		}
		if err := insert(aspectType, "Doomed", 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		if got := countFields(t, aspectType); got != 0 {
			t.Errorf("%d fields survived their workspace, want 0", got)
		}
	})

	t.Run("aspect_type_id leads an index and position follows", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute lead
                     ON lead.attrelid = i.indrelid
                    AND lead.attnum = i.indkey[0]
                   JOIN pg_attribute next
                     ON next.attrelid = i.indrelid
                    AND next.attnum = i.indkey[1]
                  WHERE i.indrelid = 'aspect_type_field'::regclass
                    AND lead.attname = 'aspect_type_id'
                    AND next.attname = 'position'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on aspect_type_field leads with aspect_type_id " +
				"followed by position, so either the foreign key is unindexed " +
				"or the editor's ordered read is not served")
		}
	})

	// The seam, from the only side this package can see it: an id generated
	// here is a uuid, and it is what document.body will use as a JSON key.
	// The end-to-end assertion lives in internal/document.
	t.Run("a field id is a uuid usable as a document body key", func(t *testing.T) {
		aspectType := newAspectType(t, "seam-shape")

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position)
             VALUES ($1::uuid, 'Methods', 1) RETURNING id::text`, aspectType,
		).Scan(&id); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if len(id) != 36 {
			t.Errorf("field id %q is %d characters, want a 36-character uuid - "+
				"document.body keys on this value", id, len(id))
		}
	})
}

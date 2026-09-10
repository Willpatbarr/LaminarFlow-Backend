package document

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newAspectTypeField creates a team, an aspect type and one field on it, and
// returns the field's ID. Names carry the test's label so two tests in one run
// cannot collide on team's UNIQUE (workspace_id, name).
func newAspectTypeField(t *testing.T, pool *pgxpool.Pool, workspaceID, label string) string {
	t.Helper()

	ctx := context.Background()

	var teamID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2)
         RETURNING id::text`, workspaceID, "Team "+label,
	).Scan(&teamID); err != nil {
		t.Fatalf("create team: %v", err)
	}

	var aspectTypeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, 'Class')
         RETURNING id::text`, teamID,
	).Scan(&aspectTypeID); err != nil {
		t.Fatalf("create aspect type: %v", err)
	}

	var fieldID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO aspect_type_field (aspect_type_id, label, position)
         VALUES ($1::uuid, 'Methods', 1) RETURNING id::text`, aspectTypeID,
	).Scan(&fieldID); err != nil {
		t.Fatalf("create aspect type field: %v", err)
	}

	return fieldID
}

// LAM-22's central claim, which no other test in the repo covers: a field ID
// from aspect_type_field is *exactly* the key used in document.body, and so
// exactly the value that lands in search_index.field_id.
//
// This lives here rather than in internal/migrate beside the other schema
// tests because TestNoSQLOutsideThisPackage bars any other package from
// issuing SQL against document or search_index. That is the rule working as
// intended - the seam can only be observed from the package that owns the
// document side of it.
//
// What this does NOT prove is that the document uses that aspect type.
// document has no aspect_type_id column; 0001 built it with nothing but a
// jsonb body and LAM-23 owns auditing it. Until that column exists there is
// no way to ask "is this field's type the document's type", so the rest of
// LAM-22 step 4 belongs to LAM-23.
func TestFieldIDIsTheDocumentBodyKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)
	fieldID := newAspectTypeField(t, pool, ws, "seam")

	svc := NewService(pool)

	docID, err := svc.Save(ctx, ws, "", map[string]any{
		fieldID: "func Save(ctx context.Context) error",
	})
	if err != nil {
		t.Fatalf("save a document keyed by a real field ID: %v", err)
	}

	// The body must hold the field ID verbatim as a JSON key.
	var hasKey bool
	if err := pool.QueryRow(ctx,
		`SELECT body ? $1 FROM document WHERE id = $2::uuid`, fieldID, docID,
	).Scan(&hasKey); err != nil {
		t.Fatalf("read the document body: %v", err)
	}
	if !hasKey {
		t.Errorf("document body has no key %q - the field ID is not the body key", fieldID)
	}

	// ...and it must arrive in search_index unchanged. A uuid that got
	// reformatted anywhere along the way would break the join this seam
	// exists to make possible.
	index := indexSnapshot(t, pool)
	content, indexed := index[docID+"|"+fieldID]
	if !indexed {
		t.Fatalf("search_index has no row for field %q; it holds %v", fieldID, index)
	}
	if content != "func Save(ctx context.Context) error" {
		t.Errorf("indexed content = %q, want the field's text", content)
	}
}

// Renaming a field's label must not disturb any document already written.
// This is the entire reason document.body keys on id rather than on label, and
// it is worth an explicit test because the alternative design - a text slug
// for an ID - would silently fail it.
func TestRenamingAFieldLabelLeavesDocumentsAlone(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)
	fieldID := newAspectTypeField(t, pool, ws, "rename")

	svc := NewService(pool)

	docID, err := svc.Save(ctx, ws, "", map[string]any{fieldID: "unchanged"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	before := indexSnapshot(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE aspect_type_field SET label = 'Operations' WHERE id = $1::uuid`, fieldID,
	); err != nil {
		t.Fatalf("rename the field: %v", err)
	}

	after := indexSnapshot(t, pool)
	if after[docID+"|"+fieldID] != before[docID+"|"+fieldID] {
		t.Errorf("renaming a label changed the indexed content: %q -> %q",
			before[docID+"|"+fieldID], after[docID+"|"+fieldID])
	}

	var stillThere bool
	if err := pool.QueryRow(ctx,
		`SELECT body ? $1 FROM document WHERE id = $2::uuid`, fieldID, docID,
	).Scan(&stillThere); err != nil {
		t.Fatalf("read the document body: %v", err)
	}
	if !stillThere {
		t.Error("renaming a field label orphaned the document body key")
	}
}

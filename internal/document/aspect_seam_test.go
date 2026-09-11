package document

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newAspectType creates a team in workspaceID and one aspect type on it, and
// returns the aspect type's ID. Team names carry the caller's label so two
// tests in one run cannot collide on team's UNIQUE (workspace_id, name).
func newAspectType(t *testing.T, pool *pgxpool.Pool, workspaceID, label string) string {
	t.Helper()

	ctx := context.Background()

	var teamID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2)
         RETURNING id::text`, workspaceID, "Team "+label,
	).Scan(&teamID); err != nil {
		t.Fatalf("create team: %v", err)
	}

	return newAspectTypeOn(t, pool, teamID)
}

// newAspectTypeOn creates one aspect type on an existing team.
func newAspectTypeOn(t *testing.T, pool *pgxpool.Pool, teamID string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, 'Class')
         RETURNING id::text`, teamID,
	).Scan(&id); err != nil {
		t.Fatalf("create aspect type: %v", err)
	}

	return id
}

// newAspectTypeField creates a team, an aspect type and one field on it, and
// returns the field's ID. Names carry the test's label so two tests in one run
// cannot collide on team's UNIQUE (workspace_id, name).
func newAspectTypeField(t *testing.T, pool *pgxpool.Pool, workspaceID, label string) string {
	t.Helper()

	ctx := context.Background()

	aspectTypeID := newAspectType(t, pool, workspaceID, label)

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

	docID, err := svc.Save(ctx, SaveParams{
		WorkspaceID: ws,
		Body: map[string]any{
			fieldID: "func Save(ctx context.Context) error",
		},
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

	docID, err := svc.Save(ctx, SaveParams{
		WorkspaceID: ws,
		Body:        map[string]any{fieldID: "unchanged"},
	})
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

// LAM-23 step 4: one normal document and one aspect document, both saved and
// read back through the shared write-path service.
//
// This is what the widened SaveParams bought. Before LAM-23 the service could
// only write the body, so an aspect document could not be created through it
// at all - the columns that make it an aspect document were unreachable.
func TestSaveWritesNormalAndAspectDocuments(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	aspectTypeID := newAspectType(t, pool, ws, "both-kinds")

	normalID, err := svc.Save(ctx, SaveParams{
		WorkspaceID: ws,
		Title:       "Meeting notes",
		Body:        map[string]any{"f_note": "shipped LAM-23"},
	})
	if err != nil {
		t.Fatalf("save a normal document: %v", err)
	}

	aspectID, err := svc.Save(ctx, SaveParams{
		WorkspaceID:  ws,
		Title:        "User Service",
		Type:         TypeAspect,
		AspectTypeID: aspectTypeID,
		Body:         map[string]any{"f_note": "handles auth"},
	})
	if err != nil {
		t.Fatalf("save an aspect document: %v", err)
	}

	read := func(t *testing.T, id string) (title, docType string, aspectType *string) {
		t.Helper()

		if err := pool.QueryRow(ctx,
			`SELECT title, type, aspect_type_id::text FROM document WHERE id = $1::uuid`, id,
		).Scan(&title, &docType, &aspectType); err != nil {
			t.Fatalf("read document %s: %v", id, err)
		}

		return title, docType, aspectType
	}

	title, docType, aspectType := read(t, normalID)
	if title != "Meeting notes" {
		t.Errorf("normal document title = %q, want %q", title, "Meeting notes")
	}
	// Type was left unset by the caller, so normalize must have filled it in.
	// Empty here would violate document_type_is_known on the next update.
	if docType != TypeNormal {
		t.Errorf("normal document type = %q, want %q", docType, TypeNormal)
	}
	if aspectType != nil {
		t.Errorf("normal document carries aspect_type_id %q, want NULL", *aspectType)
	}

	title, docType, aspectType = read(t, aspectID)
	if title != "User Service" {
		t.Errorf("aspect document title = %q, want %q", title, "User Service")
	}
	if docType != TypeAspect {
		t.Errorf("aspect document type = %q, want %q", docType, TypeAspect)
	}
	if aspectType == nil || *aspectType != aspectTypeID {
		t.Errorf("aspect document aspect_type_id = %v, want %q", aspectType, aspectTypeID)
	}

	// Both bodies still reach search_index - widening the write path must not
	// have disturbed the half LAM-3 built.
	index := indexSnapshot(t, pool)
	if index[normalID+"|f_note"] != "shipped LAM-23" {
		t.Errorf("normal document not indexed: %v", index)
	}
	if index[aspectID+"|f_note"] != "handles auth" {
		t.Errorf("aspect document not indexed: %v", index)
	}
}

// An update must not silently blank the columns it does not mention. This is
// the failure mode a params struct invites: a caller that sets only Body on
// an update would previously have left title and type alone, and now writes
// whatever the zero value is.
func TestUpdatingADocumentKeepsWhatTheCallerRestates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	id, err := svc.Save(ctx, SaveParams{
		WorkspaceID: ws,
		Title:       "Original",
		Body:        map[string]any{"f_note": "first"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := svc.Save(ctx, SaveParams{
		WorkspaceID: ws,
		ID:          id,
		Title:       "Original",
		Body:        map[string]any{"f_note": "second"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var title, docType string
	if err := pool.QueryRow(ctx,
		`SELECT title, type FROM document WHERE id = $1::uuid`, id,
	).Scan(&title, &docType); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if title != "Original" {
		t.Errorf("title = %q after update, want %q", title, "Original")
	}
	// normalize() has to run on updates too, or an unset Type writes '' and
	// trips document_type_is_known.
	if docType != TypeNormal {
		t.Errorf("type = %q after update, want %q", docType, TypeNormal)
	}
}

package document

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes, mirroring the set internal/migrate/schema_test.go defines.
// Asserting the code rather than err != nil matters: a typo in the INSERT also
// returns an error and would otherwise read as a constraint doing its job.
const (
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
)

// wantPgError fails unless err is a Postgres error carrying code. what names
// the attempted violation, so a failure says which constraint did not hold.
func wantPgError(t *testing.T, err error, code, what string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s: statement succeeded, want SQLSTATE %s", what, code)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: got %v, want a Postgres error with SQLSTATE %s", what, err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: got SQLSTATE %s (%s), want %s", what, pgErr.Code, pgErr.Message, code)
	}
}

// The constraints 0016_document_columns.sql adds to the table 0001 and 0003
// built - scope, title, type and the aspect type reference.
//
// These are schema constraint tests and every other table's live in
// internal/migrate. This one cannot: TestNoSQLOutsideThisPackage bars any
// package but this one from issuing SQL against document, and asserting a
// CHECK requires trying to violate it. Same reason LAM-22's seam test landed
// here. The rule is worth more than the convention.
func TestDocumentConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	workspace := func(t *testing.T) string {
		t.Helper()
		return defaultWorkspace(t, pool)
	}
	newTeamIn := func(t *testing.T, workspaceID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, workspaceID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create team: %v", err)
		}

		return id
	}
	newProjectIn := func(t *testing.T, teamID string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, teamID,
		).Scan(&id); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return id
	}
	// insert writes a document with explicit scope and type. Nil pointers
	// become SQL NULL.
	insert := func(workspaceID string, project, team, aspectType *string, title, docType string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO document (workspace_id, project_id, team_id, aspect_type_id, title, type)
             VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6)`,
			workspaceID, project, team, aspectType, title, docType)
		return err
	}
	ptr := func(s string) *string { return &s }

	// The columns exist and every one of them has a working default, which is
	// the property that kept Service.Save compiling and inserting across this
	// migration. 0003 got this wrong under LAM-4 and broke every insert.
	t.Run("a document can still be written with only a workspace and a body", func(t *testing.T) {
		ws := workspace(t)

		var title, docType string
		if err := pool.QueryRow(ctx,
			`INSERT INTO document (workspace_id, body) VALUES ($1::uuid, '{}'::jsonb)
             RETURNING title, type`, ws,
		).Scan(&title, &docType); err != nil {
			t.Fatalf("insert with only the pre-LAM-23 columns: %v", err)
		}
		if title != "" {
			t.Errorf("default title = %q, want empty", title)
		}
		if docType != "normal" {
			t.Errorf("default type = %q, want normal", docType)
		}
	})

	t.Run("accepts a workspace-level document with neither scope", func(t *testing.T) {
		ws := workspace(t)

		if err := insert(ws, nil, nil, nil, "Loose notes", "normal"); err != nil {
			t.Fatalf("insert with no narrowing scope: %v", err)
		}
	})

	t.Run("accepts a project-scoped and a team-scoped document", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Scope Platform")
		project := newProjectIn(t, team)

		if err := insert(ws, ptr(project), nil, nil, "Project doc", "normal"); err != nil {
			t.Fatalf("project-scoped insert: %v", err)
		}
		if err := insert(ws, nil, ptr(team), nil, "Team doc", "normal"); err != nil {
			t.Fatalf("team-scoped insert: %v", err)
		}
	})

	// The arc. Both set would make the document's owner ambiguous.
	t.Run("rejects a document scoped to both a project and a team", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Both Platform")
		project := newProjectIn(t, team)

		err := insert(ws, ptr(project), ptr(team), nil, "Confused", "normal")
		wantPgError(t, err, checkViolation, "a document in a project and a team at once")
	})

	t.Run("rejects an unknown type", func(t *testing.T) {
		ws := workspace(t)

		err := insert(ws, nil, nil, nil, "Typo", "apsect")
		wantPgError(t, err, checkViolation, "a misspelled type")
	})

	t.Run("accepts an aspect document carrying its aspect type", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Aspect Platform")
		aspectType := newAspectTypeOn(t, pool, team)

		if err := insert(ws, nil, nil, ptr(aspectType), "User Service", "aspect"); err != nil {
			t.Fatalf("aspect insert: %v", err)
		}
	})

	// Both halves of the biconditional. The ticket only asks for the first.
	t.Run("rejects a normal document carrying an aspect type", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Normal With Type")
		aspectType := newAspectTypeOn(t, pool, team)

		err := insert(ws, nil, nil, ptr(aspectType), "Mislabelled", "normal")
		wantPgError(t, err, checkViolation, "a normal document with an aspect type")
	})

	t.Run("rejects an aspect document with no aspect type", func(t *testing.T) {
		ws := workspace(t)

		err := insert(ws, nil, nil, nil, "Shapeless", "aspect")
		wantPgError(t, err, checkViolation, "an aspect document with no aspect type")
	})

	t.Run("rejects a scope or aspect type that does not exist", func(t *testing.T) {
		ws := workspace(t)
		missing := "00000000-0000-0000-0000-000000000000"

		wantPgError(t, insert(ws, ptr(missing), nil, nil, "Ghost project", "normal"),
			foreignKeyViolation, "a project that does not exist")
		wantPgError(t, insert(ws, nil, ptr(missing), nil, "Ghost team", "normal"),
			foreignKeyViolation, "a team that does not exist")
		wantPgError(t, insert(ws, nil, nil, ptr(missing), "Ghost type", "aspect"),
			foreignKeyViolation, "an aspect type that does not exist")
	})

	// SET NULL, not CASCADE: deleting a project must not destroy documents
	// that merely happened to be filed under it.
	t.Run("deleting a project leaves its documents at workspace level", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Project Delete")
		project := newProjectIn(t, team)

		var docID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO document (workspace_id, project_id, title)
             VALUES ($1::uuid, $2::uuid, 'Survivor') RETURNING id::text`, ws, project,
		).Scan(&docID); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete project: %v", err)
		}

		var alive, scoped bool
		if err := pool.QueryRow(ctx,
			`SELECT true, project_id IS NOT NULL FROM document WHERE id = $1::uuid`, docID,
		).Scan(&alive, &scoped); err != nil {
			t.Fatalf("read the document back: %v", err)
		}
		if scoped {
			t.Error("document still carries a project_id after the project was deleted")
		}
	})

	// RESTRICT, and the reach it has. Deleting an aspect type in use is
	// blocked, which also blocks deleting the team that owns the type.
	t.Run("an aspect type in use cannot be deleted", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Restrict Platform")
		aspectType := newAspectTypeOn(t, pool, team)

		if err := insert(ws, nil, nil, ptr(aspectType), "In use", "aspect"); err != nil {
			t.Fatalf("insert: %v", err)
		}

		_, err := pool.Exec(ctx, `DELETE FROM aspect_type WHERE id = $1::uuid`, aspectType)
		wantPgError(t, err, foreignKeyViolation, "deleting an aspect type that documents use")
	})

	t.Run("deleting the team is blocked too, through its aspect types", func(t *testing.T) {
		ws := workspace(t)
		team := newTeamIn(t, ws, "Restrict Chain")
		aspectType := newAspectTypeOn(t, pool, team)

		if err := insert(ws, nil, nil, ptr(aspectType), "In use", "aspect"); err != nil {
			t.Fatalf("insert: %v", err)
		}

		// team -> aspect_type is CASCADE, but aspect_type -> document is
		// RESTRICT, so the cascade cannot complete.
		_, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team)
		wantPgError(t, err, foreignKeyViolation, "deleting a team whose aspect types are in use")
	})

	t.Run("project_id and team_id each lead an index", func(t *testing.T) {
		for _, column := range []string{"project_id", "team_id"} {
			var leads bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (
                     SELECT 1
                       FROM pg_index i
                       JOIN pg_attribute a
                         ON a.attrelid = i.indrelid
                        AND a.attnum = i.indkey[0]
                      WHERE i.indrelid = 'document'::regclass
                        AND a.attname = $1
                 )`, column,
			).Scan(&leads); err != nil {
				t.Fatalf("read indexes: %v", err)
			}
			if !leads {
				t.Errorf("no index on document leads with %s, so its ON DELETE SET NULL "+
					"scans every document", column)
			}
		}
	})
}

package document

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LAM-87: the read side, the version, the checks of a body against its type, and archive.

func addField(t *testing.T, pool *pgxpool.Pool, aspectTypeID, label string, position int) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO aspect_type_field (aspect_type_id, label, position)
		 VALUES ($1::uuid, $2, $3) RETURNING id::text`, aspectTypeID, label, position,
	).Scan(&id); err != nil {
		t.Fatalf("add field %q: %v", label, err)
	}

	return id
}

// member makes an account that belongs to workspaceID, so the tests can call the account
// scoped methods the API uses.
func member(t *testing.T, pool *pgxpool.Pool, workspaceID, email string) string {
	t.Helper()

	ctx := context.Background()

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO account (email, display_name, password_hash) VALUES ($1, $1, 'x') RETURNING id::text`, email,
	).Scan(&id); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_member (workspace_id, account_id, role) VALUES ($1::uuid, $2::uuid, 'member')`,
		workspaceID, id); err != nil {
		t.Fatalf("add member: %v", err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, id) })

	return id
}

func wantFieldError(t *testing.T, err error, kind error, field string) {
	t.Helper()

	var fe *FieldError
	if !errors.As(err, &fe) || !errors.Is(fe.Kind, kind) || fe.Field != field {
		t.Fatalf("err = %v, want %v naming %q", err, kind, field)
	}
}

func TestANormalDocumentIsVersionOneAndIndexed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Title: "Notes", Body: map[string]any{ContentKey: "# hello"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if d.Version != 1 || d.Type != TypeNormal || d.ArchivedAt != nil || d.AspectTypeID != nil {
		t.Errorf("document = %+v, want version 1, normal, live, no aspect type", d)
	}

	if got := indexSnapshot(t, pool)[d.ID+"|"+ContentKey]; got != "# hello" {
		t.Errorf("indexed content = %q, want the Markdown", got)
	}

	d2, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "Notes", Body: map[string]any{ContentKey: "bye"}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if d2.Version != 2 {
		t.Errorf("version after an update = %d, want 2", d2.Version)
	}
	if got := indexSnapshot(t, pool)[d.ID+"|"+ContentKey]; got != "bye" {
		t.Errorf("indexed content after update = %q", got)
	}
}

func TestAnAspectBodyKeyMustBeAFieldOfItsType(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	typeID := newAspectType(t, pool, ws, "keys")
	field := addField(t, pool, typeID, "Methods", 1)
	stray := "00000000-0000-4000-8000-000000000001"

	_, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Type: TypeAspect, AspectTypeID: typeID,
		Body: map[string]any{field: "ok", stray: "no"}})
	wantFieldError(t, err, ErrUnknownField, stray)

	if n := rowCount(t, pool); n != 0 {
		t.Errorf("a refused write left %d documents", n)
	}

	// An absent key is legal: a field added later is simply missing from older bodies.
	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Type: TypeAspect, AspectTypeID: typeID, Body: map[string]any{}})
	if err != nil {
		t.Fatalf("an aspect document with no values: %v", err)
	}

	// A refused update changes nothing, version included.
	_, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Type: TypeAspect, AspectTypeID: typeID,
		Body: map[string]any{stray: "no"}})
	wantFieldError(t, err, ErrUnknownField, stray)
	if got, _ := svc.Get(ctx, member(t, pool, ws, "keys@example.com"), d.ID); got.Version != 1 {
		t.Errorf("version after a refused update = %d, want 1", got.Version)
	}
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM document`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

func TestAnAspectTypeOfAnotherWorkspaceIsNotFound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)

	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name) VALUES ('Elsewhere') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM document`)
		pool.Exec(ctx, `DELETE FROM team WHERE workspace_id = $1::uuid`, other)
		pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, other)
	})

	typeID := newAspectType(t, pool, other, "foreign")

	_, err := svc.Write(ctx, SaveParams{WorkspaceID: defaultWorkspace(t, pool), Type: TypeAspect, AspectTypeID: typeID})
	if !errors.Is(err, ErrAspectTypeNotFound) {
		t.Fatalf("err = %v, want ErrAspectTypeNotFound", err)
	}
}

func TestTheShapeOfTypeAndAspectTypeIsChecked(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	_, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Type: TypeAspect})
	wantFieldError(t, err, ErrTypeShape, "aspect_type_id")

	_, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, AspectTypeID: newAspectType(t, pool, ws, "shape")})
	wantFieldError(t, err, ErrTypeShape, "aspect_type_id")
}

func TestNormalToAspectMovesTheTextIntoTheFirstField(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	typeID := newAspectType(t, pool, ws, "convert")
	// Same position: the label decides, so "Alpha" is first although it was added last.
	second := addField(t, pool, typeID, "Beta", 1)
	first := addField(t, pool, typeID, "Alpha", 1)

	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Title: "Spec",
		Body: map[string]any{ContentKey: "the text", "scratch": "dropped"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// The request carries no body: the stored text moves.
	d, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "Spec", Type: TypeAspect, AspectTypeID: typeID})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if d.Type != TypeAspect || d.Version != 2 {
		t.Errorf("document = %+v, want an aspect document at version 2", d)
	}
	if len(d.Body) != 1 || d.Body[first] != "the text" {
		t.Errorf("body = %v, want only {%s: the text}", d.Body, first)
	}

	if got := indexSnapshot(t, pool); got[d.ID+"|"+first] != "the text" || got[d.ID+"|"+ContentKey] != "" {
		t.Errorf("index = %v, want the text under the first field and nothing under content", got)
	}

	// A body that fills the first field itself wins over the stored text.
	d, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, Title: "Two", Body: map[string]any{ContentKey: "stored"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	d, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Type: TypeAspect, AspectTypeID: typeID,
		Body: map[string]any{first: "chosen", second: "other"}})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if d.Body[first] != "chosen" || d.Body[second] != "other" {
		t.Errorf("body = %v, want the request's values", d.Body)
	}
}

func TestNormalToAspectOfATypeWithNoFieldsIsRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	empty := newAspectType(t, pool, ws, "no-fields")

	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Body: map[string]any{ContentKey: "text"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Type: TypeAspect, AspectTypeID: empty})
	wantFieldError(t, err, ErrNoFields, "aspect_type_id")

	// Creating one directly as an aspect of that type is fine: there is nothing to move.
	if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Type: TypeAspect, AspectTypeID: empty}); err != nil {
		t.Errorf("create an aspect document of a type with no fields: %v", err)
	}
}

func TestAStaleVersionIsAConflictCarryingTheCurrentCopy(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)

	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Title: "v1"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	one, two := 1, 2

	d, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "v2", IfVersion: &one})
	if err != nil || d.Version != 2 {
		t.Fatalf("update at the current version: %v, version %d", err, d.Version)
	}

	_, err = svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "lost", IfVersion: &one})
	var c *Conflict
	if !errors.As(err, &c) || c.Archived || c.Current.Version != 2 || c.Current.Title != "v2" {
		t.Fatalf("err = %v, want a conflict carrying version 2", err)
	}

	// Without a precondition the write is unconditional.
	if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "forced"}); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}
	if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, Title: "x", IfVersion: &two}); !errors.As(err, &c) {
		t.Errorf("version 2 after the third write: err = %v, want a conflict", err)
	}
}

func TestArchiveHidesReindexesAndConflicts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)
	me := member(t, pool, ws, "archiver@example.com")

	d, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Title: "Doomed", Body: map[string]any{ContentKey: "findable"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	stale := 7
	var c *Conflict
	if _, err := svc.Archive(ctx, me, d.ID, &stale); !errors.As(err, &c) || c.Archived {
		t.Fatalf("archive at a stale version: %v, want a conflict", err)
	}

	a, err := svc.Archive(ctx, me, d.ID, nil)
	if err != nil || a.ArchivedAt == nil || a.Version != 2 {
		t.Fatalf("archive: %+v, %v", a, err)
	}

	if _, err := svc.Get(ctx, me, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get of an archived document: %v, want ErrNotFound", err)
	}
	if got := indexSnapshot(t, pool); len(got) != 0 {
		t.Errorf("an archived document left index rows: %v", got)
	}

	// Archived: a plain write is not found; a conditional one hears that it is gone.
	if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unconditional write to an archived document: %v, want ErrNotFound", err)
	}
	two := 2
	if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, ID: d.ID, IfVersion: &two}); !errors.As(err, &c) || !c.Archived {
		t.Errorf("conditional write to an archived document: %v, want an archived conflict", err)
	}
	if _, err := svc.Archive(ctx, me, d.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("archiving twice: %v, want ErrNotFound", err)
	}

	u, err := svc.Unarchive(ctx, me, d.ID, &two)
	if err != nil || u.ArchivedAt != nil || u.Version != 3 {
		t.Fatalf("unarchive: %+v, %v", u, err)
	}
	if got := indexSnapshot(t, pool)[d.ID+"|"+ContentKey]; got != "findable" {
		t.Errorf("an unarchived document is indexed as %q, want its text back", got)
	}
	if _, err := svc.Unarchive(ctx, me, d.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unarchiving a live document: %v, want ErrNotFound", err)
	}
}

func TestScopeAndListing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)
	me := member(t, pool, ws, "lister@example.com")
	outsider := member(t, pool, ws, "outsider@example.com")
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_member WHERE account_id = $1::uuid`, outsider); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	var teamID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Lists') RETURNING id::text`, ws).Scan(&teamID); err != nil {
		t.Fatalf("team: %v", err)
	}
	typeID := newAspectTypeOn(t, pool, teamID)

	mk := func(p SaveParams) Document {
		t.Helper()
		p.AccountID = me
		d, err := svc.Write(ctx, p)
		if err != nil {
			t.Fatalf("write %+v: %v", p, err)
		}
		return d
	}
	plain := mk(SaveParams{Title: "plain"})
	filed := mk(SaveParams{Title: "filed", TeamID: teamID})
	typed := mk(SaveParams{Title: "typed", Type: TypeAspect, AspectTypeID: typeID})

	if plain.WorkspaceID != ws || filed.TeamID == nil || *filed.TeamID != teamID {
		t.Errorf("workspace/scope: plain in %s, filed under %v", plain.WorkspaceID, filed.TeamID)
	}

	if _, err := svc.Write(ctx, SaveParams{AccountID: me, TeamID: teamID, ProjectID: teamID}); err == nil {
		t.Error("both team_id and project_id were accepted")
	}
	_, err := svc.Write(ctx, SaveParams{AccountID: me, ProjectID: "00000000-0000-4000-8000-000000000002"})
	wantFieldError(t, err, ErrScopeNotFound, "project_id")

	titles := func(p ListParams) []string {
		t.Helper()
		page, err := svc.List(ctx, me, p)
		if err != nil {
			t.Fatalf("list %+v: %v", p, err)
		}
		var out []string
		for _, d := range page.Documents {
			out = append(out, d.Title)
		}
		return out
	}
	if got := titles(ListParams{}); len(got) != 3 {
		t.Errorf("unfiltered list = %v, want all three", got)
	}
	if got := titles(ListParams{TeamID: teamID}); len(got) != 1 || got[0] != "filed" {
		t.Errorf("team filter = %v", got)
	}
	if got := titles(ListParams{Type: TypeAspect}); len(got) != 1 || got[0] != "typed" {
		t.Errorf("type filter = %v", got)
	}
	if got := titles(ListParams{AspectTypeID: typeID}); len(got) != 1 || got[0] != "typed" {
		t.Errorf("aspect type filter = %v", got)
	}

	// Paging walks every document exactly once.
	var seen []string
	cursor := ""
	for range 5 {
		page, err := svc.List(ctx, me, ListParams{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, d := range page.Documents {
			seen = append(seen, d.ID)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("paging saw %v, want 3 distinct documents", seen)
	}

	if _, err := svc.Archive(ctx, me, typed.ID, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := titles(ListParams{}); len(got) != 2 {
		t.Errorf("list after an archive = %v, want two", got)
	}
	if got := titles(ListParams{Archived: true}); len(got) != 1 || got[0] != "typed" {
		t.Errorf("archived list = %v", got)
	}

	// Someone outside the workspace sees and touches nothing.
	if page, err := svc.List(ctx, outsider, ListParams{}); err != nil || len(page.Documents) != 0 {
		t.Errorf("outsider list = %v, %v", page.Documents, err)
	}
	if _, err := svc.Get(ctx, outsider, plain.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider get: %v, want ErrNotFound", err)
	}
	if _, err := svc.Write(ctx, SaveParams{AccountID: outsider, ID: plain.ID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider update: %v, want ErrNotFound", err)
	}
	if _, err := svc.Archive(ctx, outsider, plain.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider archive: %v, want ErrNotFound", err)
	}
}

func TestFieldUsageCountsDocumentsHoldingTheKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	svc := NewService(pool)
	me := member(t, pool, ws, "usage@example.com")

	typeID := newAspectType(t, pool, ws, "usage")
	used := addField(t, pool, typeID, "Used", 1)
	unused := addField(t, pool, typeID, "Unused", 2)

	for _, body := range []map[string]any{{used: "a"}, {used: "b"}, {}} {
		if _, err := svc.Write(ctx, SaveParams{WorkspaceID: ws, Type: TypeAspect, AspectTypeID: typeID, Body: body}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for field, want := range map[string]int{used: 2, unused: 0} {
		if n, err := svc.CountFieldUsage(ctx, me, typeID, field); err != nil || n != want {
			t.Errorf("usage of %s = %d, %v; want %d", field, n, err, want)
		}
	}
}

package aspect_test

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An external test package, so these can read document to prove the thing this whole
// design is for: a field edit changes no document. internal/aspect itself never names
// that table, and sqlguard would fail the build if it did.

var testDatabaseURL string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		return m.Run()
	}
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	defer cleanup()

	testDatabaseURL = dsn

	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		log.Printf("load migrations: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("connect: %v", err)
		return 1
	}
	if _, err := migrate.Up(ctx, pool, loaded); err != nil {
		pool.Close()
		log.Printf("apply migrations: %v", err)
		return 1
	}
	pool.Close()

	return m.Run()
}

// world is two workspaces with a member each, so every scoping assertion has a caller
// who should see the type and one who should not.
type world struct {
	pool           *pgxpool.Pool
	svc            *aspect.Service
	member, outsid string
	team           string
	workspace      string
}

func newWorld(t *testing.T) world {
	t.Helper()

	if testDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, table := range []string{"document", "team", "account"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}

	one := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		return id
	}

	w := world{pool: pool, svc: aspect.NewService(pool)}
	w.workspace = one(`SELECT id::text FROM workspace WHERE name = 'Default'`)
	other := one(`INSERT INTO workspace (name) VALUES ('Other') RETURNING id::text`)

	w.member = one(`INSERT INTO account (email, password_hash, display_name)
	                VALUES ('member-' || gen_random_uuid() || '@x.com', 'h', 'Member')
	                RETURNING id::text`)
	w.outsid = one(`INSERT INTO account (email, password_hash, display_name)
	                VALUES ('outsider-' || gen_random_uuid() || '@x.com', 'h', 'Outsider')
	                RETURNING id::text`)

	one(`INSERT INTO workspace_member (workspace_id, account_id, role)
	     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, w.workspace, w.member)
	// A real member of somewhere else, so these prove scoping rather than merely
	// that an account with no memberships sees nothing.
	one(`INSERT INTO workspace_member (workspace_id, account_id, role)
	     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, other, w.outsid)

	w.team = one(`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')
	              RETURNING id::text`, w.workspace)

	return w
}

func (w world) newType(t *testing.T, name string) aspect.Type {
	t.Helper()

	got, err := w.svc.CreateType(context.Background(), w.member, w.team, name)
	if err != nil {
		t.Fatalf("create type: %v", err)
	}

	return got
}

func (w world) addFields(t *testing.T, typeID string, labels ...string) []aspect.Field {
	t.Helper()

	out := make([]aspect.Field, len(labels))
	for i, label := range labels {
		f, err := w.svc.AddField(context.Background(), w.member, typeID, label)
		if err != nil {
			t.Fatalf("add field %q: %v", label, err)
		}
		out[i] = f
	}

	return out
}

func labels(fields []aspect.Field) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.Label
	}

	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestCreateThenGet(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")

	got, err := w.svc.Get(context.Background(), w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Class" || got.TeamID != w.team {
		t.Errorf("got %+v, want the type just created", got)
	}
	// A type with no fields is legal, and the editor starts from one.
	if len(got.Fields) != 0 {
		t.Errorf("a new type has %d fields, want none", len(got.Fields))
	}
}

func TestFieldsAppendInOrder(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")
	added := w.addFields(t, made.ID, "Methods", "Properties", "Notes")

	for i, f := range added {
		if f.Position != i+1 {
			t.Errorf("%q landed at position %d, want %d", f.Label, f.Position, i+1)
		}
	}

	got, err := w.svc.Get(context.Background(), w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := []string{"Methods", "Properties", "Notes"}; !equal(labels(got.Fields), want) {
		t.Errorf("fields = %v, want %v", labels(got.Fields), want)
	}
}

// The property the whole package rests on: a field's id is what document.body keys
// on, so nothing here may reassign it.
func TestRenamingAFieldKeepsItsID(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")
	before := w.addFields(t, made.ID, "Methods")[0]

	after, err := w.svc.RenameField(context.Background(), w.member, before.ID, "Operations")
	if err != nil {
		t.Fatalf("rename field: %v", err)
	}

	if after.ID != before.ID {
		t.Errorf("the id changed from %s to %s - every document keyed on the old one is orphaned",
			before.ID, after.ID)
	}
	if after.Label != "Operations" {
		t.Errorf("label = %q, want the new one", after.Label)
	}
	if after.Position != before.Position {
		t.Errorf("a rename moved the field from %d to %d", before.Position, after.Position)
	}
}

// Reorder is one statement writing every position. 0015 declines a unique index on
// (aspect_type_id, position) so that this needs no temporary values.
func TestReorderWritesEveryPosition(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")
	f := w.addFields(t, made.ID, "Methods", "Properties", "Notes")

	got, err := w.svc.Reorder(context.Background(), w.member, made.ID,
		[]string{f[2].ID, f[0].ID, f[1].ID})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}

	if want := []string{"Notes", "Methods", "Properties"}; !equal(labels(got), want) {
		t.Errorf("order = %v, want %v", labels(got), want)
	}
	for i, field := range got {
		if field.Position != i+1 {
			t.Errorf("%q is at position %d, want %d", field.Label, field.Position, i+1)
		}
	}

	// And it survives a reread, so the transaction committed what it returned.
	reread, err := w.svc.Get(context.Background(), w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !equal(labels(reread.Fields), labels(got)) {
		t.Errorf("reread = %v, want %v", labels(reread.Fields), labels(got))
	}
}

// A swap is the case a unique index on (aspect_type_id, position) would have broken
// halfway through. It is a plain update here.
func TestASwapOfTwoFieldsNeedsNoTemporaryPosition(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")
	f := w.addFields(t, made.ID, "Methods", "Properties")

	got, err := w.svc.Reorder(context.Background(), w.member, made.ID,
		[]string{f[1].ID, f[0].ID})
	if err != nil {
		t.Fatalf("swap: %v", err)
	}

	if want := []string{"Properties", "Methods"}; !equal(labels(got), want) {
		t.Errorf("order = %v, want %v", labels(got), want)
	}
}

// A partial list would renumber some fields and leave others, which is the one input
// here that corrupts an ordering rather than failing.
func TestReorderRejectsAnIncompleteSet(t *testing.T) {
	w := newWorld(t)
	made := w.newType(t, "Class")
	f := w.addFields(t, made.ID, "Methods", "Properties", "Notes")

	for name, ids := range map[string][]string{
		"missing one":    {f[0].ID, f[1].ID},
		"a duplicate":    {f[0].ID, f[0].ID, f[1].ID},
		"a stranger":     {f[0].ID, f[1].ID, made.ID},
		"nothing at all": {},
	} {
		_, err := w.svc.Reorder(context.Background(), w.member, made.ID, ids)
		if !errors.Is(err, aspect.ErrFieldSetWrong) {
			t.Errorf("%s: err = %v, want ErrFieldSetWrong", name, err)
		}
	}

	// And nothing moved.
	got, err := w.svc.Get(context.Background(), w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := []string{"Methods", "Properties", "Notes"}; !equal(labels(got.Fields), want) {
		t.Errorf("a rejected reorder still changed the order to %v", labels(got.Fields))
	}
}

// LAM-44's delete decision, asserted rather than only written down.
func TestRemovingAFieldLeavesTheValueInEveryDocument(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newType(t, "Class")
	f := w.addFields(t, made.ID, "Methods")[0]

	// A document of that type, holding a value under the field's id.
	var docID string
	err := w.pool.QueryRow(ctx,
		`INSERT INTO document (workspace_id, team_id, aspect_type_id, title, type, body)
		 SELECT $1::uuid, $2::uuid, $3::uuid, 'Parser', 'aspect',
		        jsonb_build_object($4::text, 'parse(), lex()')
		 RETURNING id::text`, w.workspace, w.team, made.ID, f.ID).Scan(&docID)
	if err != nil {
		t.Fatalf("fixture document: %v", err)
	}

	if err := w.svc.RemoveField(ctx, w.member, f.ID); err != nil {
		t.Fatalf("remove field: %v", err)
	}

	var value *string
	if err := w.pool.QueryRow(ctx,
		`SELECT body ->> $2 FROM document WHERE id = $1::uuid`, docID, f.ID).Scan(&value); err != nil {
		t.Fatalf("read body: %v", err)
	}

	if value == nil || *value != "parse(), lex()" {
		t.Errorf("the value under the removed field is %v, want it left in place - "+
			"stripping it would be unrecoverable", value)
	}

	// The field itself is gone, so nothing renders it.
	got, err := w.svc.Get(ctx, w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Fields) != 0 {
		t.Errorf("the type still has %d fields", len(got.Fields))
	}
}

// Adding a field touches no document either - it appears as a key that is absent,
// which renders empty.
func TestAddingAFieldRewritesNoDocument(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newType(t, "Class")

	var before string
	err := w.pool.QueryRow(ctx,
		`INSERT INTO document (workspace_id, team_id, aspect_type_id, title, type)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'Parser', 'aspect')
		 RETURNING updated_at::text`, w.workspace, w.team, made.ID).Scan(&before)
	if err != nil {
		t.Fatalf("fixture document: %v", err)
	}

	w.addFields(t, made.ID, "Methods")

	var after string
	if err := w.pool.QueryRow(ctx,
		`SELECT updated_at::text FROM document WHERE aspect_type_id = $1::uuid`,
		made.ID).Scan(&after); err != nil {
		t.Fatalf("read document: %v", err)
	}

	if after != before {
		t.Errorf("adding a field wrote to a document: updated_at moved from %s to %s", before, after)
	}
}

// Scoping, from both sides, on every operation that takes an id.
func TestAnOutsiderReachesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newType(t, "Class")
	f := w.addFields(t, made.ID, "Methods")[0]

	if _, err := w.svc.CreateType(ctx, w.outsid, w.team, "Sneaky"); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("create as an outsider = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Get(ctx, w.outsid, made.ID); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("get = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.RenameType(ctx, w.outsid, made.ID, "Hijacked"); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("rename type = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.AddField(ctx, w.outsid, made.ID, "Sneaky"); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("add field = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.RenameField(ctx, w.outsid, f.ID, "Hijacked"); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("rename field = %v, want ErrNotFound", err)
	}
	if err := w.svc.RemoveField(ctx, w.outsid, f.ID); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("remove field = %v, want ErrNotFound", err)
	}
	// Scope is checked before the field set, or a failed reorder would tell an
	// outsider how many fields the type has.
	if _, err := w.svc.Reorder(ctx, w.outsid, made.ID, []string{f.ID}); !errors.Is(err, aspect.ErrNotFound) {
		t.Errorf("reorder = %v, want ErrNotFound", err)
	}

	// And nothing changed.
	got, err := w.svc.Get(ctx, w.member, made.ID)
	if err != nil {
		t.Fatalf("get as the member: %v", err)
	}
	if got.Name != "Class" || len(got.Fields) != 1 || got.Fields[0].Label != "Methods" {
		t.Errorf("an outsider changed something: %+v", got)
	}
}

func TestListShowsOnlyTheCallersTypes(t *testing.T) {
	w := newWorld(t)
	w.newType(t, "Class")
	w.newType(t, "Service")

	mine, err := w.svc.ListTypes(context.Background(), w.member, w.team)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mine) != 2 {
		t.Errorf("got %d types, want 2", len(mine))
	}

	theirs, err := w.svc.ListTypes(context.Background(), w.outsid, w.team)
	if err != nil {
		t.Fatalf("list as an outsider: %v", err)
	}
	if len(theirs) != 0 {
		t.Errorf("an outsider listed %d types", len(theirs))
	}
}

// 0014 declines UNIQUE (team_id, name) deliberately, so this is documented behaviour
// rather than an oversight - and LAM-43's bootstrap has to cope with it.
func TestTwoTypesMayShareAName(t *testing.T) {
	w := newWorld(t)
	w.newType(t, "Class")

	if _, err := w.svc.CreateType(context.Background(), w.member, w.team, "Class"); err != nil {
		t.Errorf("a second type named Class was refused: %v", err)
	}
}

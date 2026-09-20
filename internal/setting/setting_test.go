package setting_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

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

type world struct {
	pool            *pgxpool.Pool
	svc             *setting.Service
	member, outsid  string
	workspace, team string
	project         string
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

	for _, table := range []string{"setting", "team", "account"} {
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

	w := world{pool: pool, svc: setting.NewService(pool)}
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
	one(`INSERT INTO workspace_member (workspace_id, account_id, role)
	     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, other, w.outsid)

	w.team = one(`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')
	              RETURNING id::text`, w.workspace)
	w.project = one(`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Core')
	                 RETURNING id::text`, w.team)

	return w
}

func (w world) teamTarget() setting.Target {
	return setting.Target{Scope: setting.TeamScope, ID: w.team}
}

func TestSetThenGet(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(`14`)); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := w.svc.Get(ctx, w.member, w.teamTarget(), setting.SprintLengthDays)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "14" {
		t.Errorf("got %s, want 14", got)
	}
}

// The unique index is partial, so the upsert has to name its predicate to use it. A
// second Set that inserted instead of updating would violate the index, not overwrite.
func TestSettingTheSameKeyTwiceOverwrites(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	for _, v := range []string{`7`, `14`, `21`} {
		if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(v)); err != nil {
			t.Fatalf("set %s: %v", v, err)
		}
	}

	got, err := w.svc.Get(ctx, w.member, w.teamTarget(), setting.SprintLengthDays)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "21" {
		t.Errorf("got %s, want the last value written", got)
	}

	var rows int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM setting WHERE team_id = $1::uuid`, w.team).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d rows for one key, want 1", rows)
	}
}

// Absence is the default. Writing one would freeze today's default into the row, so
// Unset deletes rather than writing anything back.
func TestUnsetRemovesTheRow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(`14`)); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := w.svc.Unset(ctx, w.member, w.teamTarget(), setting.SprintLengthDays); err != nil {
		t.Fatalf("unset: %v", err)
	}

	if _, err := w.svc.Get(ctx, w.member, w.teamTarget(), setting.SprintLengthDays); !errors.Is(err, setting.ErrNotSet) {
		t.Errorf("get after unset = %v, want ErrNotSet", err)
	}
	if err := w.svc.Unset(ctx, w.member, w.teamTarget(), setting.SprintLengthDays); !errors.Is(err, setting.ErrNotSet) {
		t.Errorf("unset twice = %v, want ErrNotSet", err)
	}
}

// Nothing configured is the normal state, and it is not an error.
func TestAnUnsetKeyIsNotAnError(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.Get(context.Background(), w.member, w.teamTarget(), setting.SprintLengthDays)
	if !errors.Is(err, setting.ErrNotSet) {
		t.Errorf("err = %v, want ErrNotSet", err)
	}
}

// The write path is the gate. Everything the registry refuses has to be refused here
// too, before a row exists.
func TestTheWritePathRefusesWhatTheRegistryDoes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Wrong shape.
	if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(`"two weeks"`)); !errors.Is(err, setting.ErrWrongShape) {
		t.Errorf("a string sprint length = %v, want ErrWrongShape", err)
	}

	// Right key, wrong level.
	workspaceTarget := setting.Target{Scope: setting.WorkspaceScope, ID: w.workspace}
	if err := w.svc.Set(ctx, w.member, workspaceTarget, setting.SprintLengthDays, raw(`14`)); !errors.Is(err, setting.ErrWrongScope) {
		t.Errorf("a team key at workspace scope = %v, want ErrWrongScope", err)
	}

	// And nothing was written by either.
	var rows int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM setting`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d rows were written by refused writes", rows)
	}
}

func TestAnOutsiderReachesNoSettings(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(`14`)); err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := w.svc.Set(ctx, w.outsid, w.teamTarget(), setting.SprintLengthDays, raw(`99`)); !errors.Is(err, setting.ErrNotFound) {
		t.Errorf("set as an outsider = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Get(ctx, w.outsid, w.teamTarget(), setting.SprintLengthDays); !errors.Is(err, setting.ErrNotFound) {
		t.Errorf("get = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.List(ctx, w.outsid, w.teamTarget()); !errors.Is(err, setting.ErrNotFound) {
		t.Errorf("list = %v, want ErrNotFound", err)
	}
	if err := w.svc.Unset(ctx, w.outsid, w.teamTarget(), setting.SprintLengthDays); !errors.Is(err, setting.ErrNotFound) {
		t.Errorf("unset = %v, want ErrNotFound", err)
	}

	got, err := w.svc.Get(ctx, w.member, w.teamTarget(), setting.SprintLengthDays)
	if err != nil || string(got) != "14" {
		t.Errorf("an outsider changed the value: %s, %v", got, err)
	}
}

// LAM-52 decision 3. A row written before this package existed, or by hand, must not
// break the settings screen for everyone.
func TestAnUnregisteredRowIsSkippedOnReadAndReportedSeparately(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if err := w.svc.Set(ctx, w.member, w.teamTarget(), setting.SprintLengthDays, raw(`14`)); err != nil {
		t.Fatalf("set: %v", err)
	}
	// The row the registry exists to prevent, written around it - which is what a
	// pre-existing database looks like.
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO setting (team_id, key, value) VALUES ($1::uuid, 'kanban_colums', '["status"]')`,
		w.team); err != nil {
		t.Fatalf("fixture unregistered row: %v", err)
	}

	list, err := w.svc.List(ctx, w.member, w.teamTarget())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list returned %d settings, want only the registered one", len(list))
	}
	if _, ok := list[setting.SprintLengthDays]; !ok {
		t.Error("the registered setting is missing from the list")
	}

	// Skipped, not hidden.
	unknown, err := w.svc.Unregistered(ctx)
	if err != nil {
		t.Fatalf("unregistered: %v", err)
	}
	if len(unknown) != 1 || unknown[0].Key != "kanban_colums" {
		t.Fatalf("unregistered = %+v, want the one bad row", unknown)
	}
	if unknown[0].Scope != setting.TeamScope || unknown[0].TargetID != w.team {
		t.Errorf("the report does not say where the row is: %+v", unknown[0])
	}
}

// The registry's menu of column sources and board.group_by's CHECK are two closed sets
// that have to hold the same values. Offering a source the CHECK refuses would let
// someone configure a board that cannot be created.
func TestEveryColumnSourceIsAcceptedByTheBoardCheck(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	def := setting.Registry[setting.BoardColumnSources]

	// Each registered source must pass the CHECK.
	for _, source := range []string{setting.SourceStatus} {
		if err := def.Validate(raw(`["` + source + `"]`)); err != nil {
			t.Fatalf("%q is not a registered source: %v", source, err)
		}

		if _, err := w.pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name, group_by)
			 VALUES ($1::uuid, $2::uuid, $3, $4)`,
			w.project, w.team, "Board "+source, source); err != nil {
			t.Errorf("the registry offers %q but board.group_by's CHECK refuses it: %v", source, err)
		}
	}

	// And the reverse: a value the registry refuses is one the CHECK refuses too, so
	// the menu is not merely a subset that has drifted narrow.
	for _, notASource := range []string{"label", "assignee", "statuses"} {
		if err := def.Validate(raw(`["` + notASource + `"]`)); err == nil {
			t.Errorf("the registry offers %q", notASource)
		}

		_, err := w.pool.Exec(ctx,
			`INSERT INTO board (project_id, team_id, name, group_by)
			 VALUES ($1::uuid, $2::uuid, $3, $4)`,
			w.project, w.team, "Bad "+notASource, notASource)
		if err == nil {
			t.Errorf("board.group_by accepts %q, so the registry's menu is too narrow", notASource)
		}
	}
}

// Both waiting consumers can be written, which is the thing LAM-48 and 0012 were
// blocked on.
func TestBothWaitingConsumersHaveAKeyTheyCanWrite(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	for key, value := range map[setting.Key]json.RawMessage{
		setting.SprintLengthDays:   raw(`14`),
		setting.BoardColumnSources: raw(`["status"]`),
	} {
		if err := w.svc.Set(ctx, w.member, w.teamTarget(), key, value); err != nil {
			t.Errorf("%s could not be set: %v", key, err)
		}
	}

	list, err := w.svc.List(ctx, w.member, w.teamTarget())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("list returned %d settings, want both", len(list))
	}
}

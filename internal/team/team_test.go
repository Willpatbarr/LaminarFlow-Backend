package team_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// One test package for both bootstrap points, because the second depends on the first:
// a seeded board's columns map to the statuses the team bootstrap wrote, so testing
// them apart would mean faking one of them.

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
	pool           *pgxpool.Pool
	teams          *team.Service
	projects       *project.Service
	member, outsid string
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

	for _, table := range []string{"team", "account"} {
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

	w := world{pool: pool, teams: team.NewService(pool), projects: project.NewService(pool)}
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

	return w
}

func (w world) newTeam(t *testing.T, name string) team.Team {
	t.Helper()

	got, err := w.teams.Create(context.Background(), w.member,
		team.CreateParams{WorkspaceID: w.workspace, Name: name})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	return got
}

func (w world) count(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

// LAM-17 step 3 and LAM-21 step 3, discharged. This is the test the ticket asked for -
// a real per-team one, which the schema tests could not be.
func TestCreatingATeamSeedsItsStatusesAndAspectTypes(t *testing.T) {
	w := newWorld(t)
	made := w.newTeam(t, "Platform")

	if len(made.Statuses) != len(team.DefaultStatuses) {
		t.Fatalf("seeded %d statuses, want %d", len(made.Statuses), len(team.DefaultStatuses))
	}
	for i, st := range made.Statuses {
		want := team.DefaultStatuses[i]
		if st.Name != want.Name || st.Category != want.Category || st.Position != want.Position {
			t.Errorf("status %d = %+v, want %+v", i, st, want)
		}
		if st.ID == "" {
			t.Errorf("status %q has no id", st.Name)
		}
	}

	// One status per category, because reporting branches on category and a
	// workflow missing "done" cannot express finished work.
	seen := map[string]bool{}
	for _, st := range made.Statuses {
		seen[st.Category] = true
	}
	for _, category := range []string{"not_started", "in_progress", "done"} {
		if !seen[category] {
			t.Errorf("no seeded status is %s", category)
		}
	}

	if len(made.AspectTypes) != len(team.StarterTypes) {
		t.Errorf("seeded %d aspect types, want %d", len(made.AspectTypes), len(team.StarterTypes))
	}
	if n := w.count(t, `SELECT count(*) FROM aspect_type WHERE team_id = $1::uuid`, made.ID); n != 5 {
		t.Errorf("%d aspect_type rows, want 5", n)
	}
}

// LAM-50's obligation. Team-scoped, shared, and owned by nobody.
func TestCreatingATeamSeedsOneSharedView(t *testing.T) {
	w := newWorld(t)
	made := w.newTeam(t, "Platform")

	var (
		name    string
		layout  string
		shared  bool
		owner   *string
		config  json.RawMessage
		project *string
	)
	err := w.pool.QueryRow(context.Background(),
		`SELECT name, layout, is_shared, owner_account_id::text, config, project_id::text
		   FROM saved_view WHERE team_id = $1::uuid`, made.ID,
	).Scan(&name, &layout, &shared, &owner, &config, &project)
	if err != nil {
		t.Fatalf("read seeded view: %v", err)
	}

	if name != "All tickets" {
		t.Errorf("view name = %q", name)
	}
	if !shared {
		t.Error("the default view is private, so only its owner would see it")
	}
	// Owned by nobody on purpose: a team default belongs to the team, and a null
	// owner is one fewer row for account deletion to orphan.
	if owner != nil {
		t.Errorf("the default view is owned by %s", *owner)
	}
	if layout != "list" || project != nil {
		t.Errorf("layout %q project %v, want a team-wide list", layout, project)
	}

	// The config has to be the shape LAM-58 defined, or LAM-59 cannot read it.
	var parsed struct {
		Filter map[string]any `json:"filter"`
		Sort   []struct {
			Field string `json:"field"`
			Desc  bool   `json:"desc"`
		} `json:"sort"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		t.Fatalf("the seeded config is not the filter shape: %v", err)
	}
	if parsed.Filter["op"] != "and" || len(parsed.Sort) != 1 || parsed.Sort[0].Field != "updated_at" {
		t.Errorf("config = %s, want an empty and-group sorted by updated_at", config)
	}
}

// LAM-49's open question, answered by not answering it.
func TestCreatingATeamSeedsNoLabels(t *testing.T) {
	w := newWorld(t)
	made := w.newTeam(t, "Platform")

	if n := w.count(t, `SELECT count(*) FROM label WHERE team_id = $1::uuid`, made.ID); n != 0 {
		t.Errorf("%d labels were seeded - a label vocabulary is a team's own", n)
	}
}

// LAM-21's "optional bootstrap step", made explicit.
func TestABareTeamGetsNothing(t *testing.T) {
	w := newWorld(t)

	made, err := w.teams.Create(context.Background(), w.member,
		team.CreateParams{WorkspaceID: w.workspace, Name: "Imported", Bare: true})
	if err != nil {
		t.Fatalf("create bare: %v", err)
	}

	if len(made.Statuses) != 0 || len(made.AspectTypes) != 0 {
		t.Errorf("a bare team was seeded: %+v", made)
	}
	for _, table := range []string{"status", "aspect_type", "saved_view"} {
		if n := w.count(t, `SELECT count(*) FROM `+table+` WHERE team_id = $1::uuid`, made.ID); n != 0 {
			t.Errorf("%d %s rows on a bare team", n, table)
		}
	}
}

// The whole reason seeding runs inside the creation transaction.
func TestAFailedSeedCreatesNoTeam(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// A name collision is the failure available without mutating the code: the
	// insert fails, so nothing downstream of it runs and nothing is left behind.
	w.newTeam(t, "Platform")

	_, err := w.teams.Create(ctx, w.member, team.CreateParams{WorkspaceID: w.workspace, Name: "Platform"})
	if !errors.Is(err, team.ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}

	if n := w.count(t, `SELECT count(*) FROM team WHERE workspace_id = $1::uuid`, w.workspace); n != 1 {
		t.Errorf("%d teams, want 1", n)
	}
	// Three statuses, not six: the second attempt seeded nothing.
	if n := w.count(t, `SELECT count(*) FROM status`); n != 3 {
		t.Errorf("%d statuses, want 3", n)
	}
}

// 0005's UNIQUE is per workspace, so this must not be a global name reservation.
func TestTwoWorkspacesMayEachHaveAPlatformTeam(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	w.newTeam(t, "Platform")

	var other string
	if err := w.pool.QueryRow(ctx,
		`SELECT workspace_id::text FROM workspace_member WHERE account_id = $1::uuid`,
		w.outsid).Scan(&other); err != nil {
		t.Fatalf("find the other workspace: %v", err)
	}

	if _, err := w.teams.Create(ctx, w.outsid,
		team.CreateParams{WorkspaceID: other, Name: "Platform"}); err != nil {
		t.Errorf("a second workspace could not have a Platform team: %v", err)
	}
}

func TestCreatingATeamInSomeoneElsesWorkspaceCreatesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.teams.Create(ctx, w.outsid,
		team.CreateParams{WorkspaceID: w.workspace, Name: "Sneaky"}); !errors.Is(err, team.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	if n := w.count(t, `SELECT count(*) FROM team`); n != 0 {
		t.Errorf("%d teams were created by an outsider", n)
	}
	if n := w.count(t, `SELECT count(*) FROM status`); n != 0 {
		t.Errorf("%d statuses were seeded by a refused creation", n)
	}
}

// The second bootstrap point. board is project-scoped, so this cannot happen when the
// team is created - which is the scope question LAM-43 left open.
func TestCreatingAProjectSeedsABoardMappedToTheTeamsStatuses(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newTeam(t, "Platform")

	p, err := w.projects.Create(ctx, w.member, project.CreateParams{TeamID: made.ID, Name: "Core"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if p.BoardID == "" {
		t.Fatal("no board was seeded")
	}
	if len(p.Columns) != len(made.Statuses) {
		t.Fatalf("%d columns for %d statuses", len(p.Columns), len(made.Statuses))
	}

	for i, col := range p.Columns {
		st := made.Statuses[i]
		if col.Name != st.Name || col.Position != st.Position {
			t.Errorf("column %d = %+v, want it to mirror %+v", i, col, st)
		}
		if col.StatusID != st.ID {
			t.Errorf("column %q maps to %s, want %s", col.Name, col.StatusID, st.ID)
		}
	}

	// The mapping rows exist, which is what makes the board render rather than
	// merely look right in the response.
	n := w.count(t, `SELECT count(*) FROM board_column_status bcs
	                        JOIN board_column bc ON bc.id = bcs.board_column_id
	                       WHERE bc.board_id = $1::uuid`, p.BoardID)
	if n != len(made.Statuses) {
		t.Errorf("%d board_column_status rows, want %d", n, len(made.Statuses))
	}
}

// A bare team has no statuses, so a board built from them has no columns. Legal, and
// the one case where the two bootstraps visibly interact.
func TestAProjectInABareTeamGetsABoardWithNoColumns(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	bare, err := w.teams.Create(ctx, w.member,
		team.CreateParams{WorkspaceID: w.workspace, Name: "Imported", Bare: true})
	if err != nil {
		t.Fatalf("create bare team: %v", err)
	}

	p, err := w.projects.Create(ctx, w.member, project.CreateParams{TeamID: bare.ID, Name: "Core"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if p.BoardID == "" {
		t.Error("no board on a project in a bare team")
	}
	if len(p.Columns) != 0 {
		t.Errorf("%d columns from a team with no statuses", len(p.Columns))
	}
}

func TestABareProjectGetsNoBoard(t *testing.T) {
	w := newWorld(t)
	made := w.newTeam(t, "Platform")

	p, err := w.projects.Create(context.Background(), w.member,
		project.CreateParams{TeamID: made.ID, Name: "Core", Bare: true})
	if err != nil {
		t.Fatalf("create bare project: %v", err)
	}

	if p.BoardID != "" || len(p.Columns) != 0 {
		t.Errorf("a bare project was seeded: %+v", p)
	}
	if n := w.count(t, `SELECT count(*) FROM board WHERE project_id = $1::uuid`, p.ID); n != 0 {
		t.Errorf("%d boards on a bare project", n)
	}
}

func TestCreatingAProjectInAnUnreachableTeamCreatesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newTeam(t, "Platform")

	if _, err := w.projects.Create(ctx, w.outsid,
		project.CreateParams{TeamID: made.ID, Name: "Sneaky"}); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	if n := w.count(t, `SELECT count(*) FROM project WHERE team_id = $1::uuid`, made.ID); n != 0 {
		t.Errorf("%d projects were created by an outsider", n)
	}
	if n := w.count(t, `SELECT count(*) FROM board`); n != 0 {
		t.Errorf("%d boards were seeded by a refused creation", n)
	}
}

// Seeding is the service's job, not a trigger's. internal/migrate's
// TestAspectTypeConstraints/creating_a_team_seeds_no_aspect_types inserts a team with
// raw SQL and asserts nothing appears - so that test and this one only both pass if the
// seeding lives exactly where LAM-43 says it should.
func TestARawTeamInsertSeedsNothing(t *testing.T) {
	w := newWorld(t)

	var id string
	if err := w.pool.QueryRow(context.Background(),
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Trigger check')
		 RETURNING id::text`, w.workspace).Scan(&id); err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	for _, table := range []string{"status", "aspect_type", "saved_view"} {
		if n := w.count(t, `SELECT count(*) FROM `+table+` WHERE team_id = $1::uuid`, id); n != 0 {
			t.Errorf("%d %s rows appeared from a raw team insert - seeding is in a trigger", n, table)
		}
	}
}

// Everything seeded is an ordinary row. Master spec 3.4 - no status is sacred - and
// LAM-21's note that starter types are not system types.
func TestEverythingSeededIsAnOrdinaryRow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	made := w.newTeam(t, "Platform")

	for _, st := range made.Statuses {
		if _, err := w.pool.Exec(ctx, `DELETE FROM status WHERE id = $1::uuid`, st.ID); err != nil {
			t.Errorf("a seeded status could not be deleted: %v", err)
		}
	}
	if _, err := w.pool.Exec(ctx,
		`DELETE FROM aspect_type WHERE team_id = $1::uuid`, made.ID); err != nil {
		t.Errorf("seeded aspect types could not be deleted: %v", err)
	}
	if _, err := w.pool.Exec(ctx,
		`DELETE FROM saved_view WHERE team_id = $1::uuid`, made.ID); err != nil {
		t.Errorf("the seeded view could not be deleted: %v", err)
	}

	if n := w.count(t, `SELECT count(*) FROM team WHERE id = $1::uuid`, made.ID); n != 1 {
		t.Error("deleting the seeded rows took the team with it")
	}
}

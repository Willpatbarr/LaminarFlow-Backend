package team_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
)

// LAM-64. The two list services live in this test package for the reason the file above
// gives: a project list needs teams, and faking them would test the fake.

// otherWorkspace is the workspace newWorld put the outsider in.
func (w world) otherWorkspace(t *testing.T) string {
	t.Helper()

	var id string
	if err := w.pool.QueryRow(context.Background(),
		`SELECT workspace_id::text FROM workspace_member WHERE account_id = $1::uuid`,
		w.outsid).Scan(&id); err != nil {
		t.Fatalf("find the other workspace: %v", err)
	}

	return id
}

// outsiderTeam makes a team in the workspace the member cannot reach.
func (w world) outsiderTeam(t *testing.T, name string) team.Team {
	t.Helper()

	got, err := w.teams.Create(context.Background(), w.outsid,
		team.CreateParams{WorkspaceID: w.otherWorkspace(t), Name: name})
	if err != nil {
		t.Fatalf("create outsider team: %v", err)
	}

	return got
}

func (w world) newProject(t *testing.T, teamID, name string) project.Project {
	t.Helper()

	got, err := w.projects.Create(context.Background(), w.member,
		project.CreateParams{TeamID: teamID, Name: name})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	return got
}

func (w world) listTeams(t *testing.T, as string, p team.ListParams) team.Page {
	t.Helper()

	page, err := w.teams.List(context.Background(), as, p)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}

	return page
}

func (w world) listProjects(t *testing.T, as string, p project.ListParams) project.Page {
	t.Helper()

	page, err := w.projects.List(context.Background(), as, p)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}

	return page
}

func teamNames(page team.Page) []string {
	out := make([]string, len(page.Teams))
	for i, tm := range page.Teams {
		out[i] = tm.Name
	}

	return out
}

func projectIDs(page project.Page) []string {
	out := make([]string, len(page.Projects))
	for i, p := range page.Projects {
		out[i] = p.ID
	}

	return out
}

func nameIs(op filter.Operator, v any) filter.Group {
	return filter.Group{Op: filter.And, Conditions: []filter.Condition{{Field: "name", Op: op, Value: v}}}
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

// ─── teams ────────────────────────────────────────────────────────────────────

// The point of the ticket's step 2: a client enumerating teams needs no third call,
// because each team arrives with the statuses it will group tickets by.
func TestTheTeamListCarriesEachTeamsStatusesWithCategoryAndOrder(t *testing.T) {
	w := newWorld(t)
	made := w.newTeam(t, "Platform")

	page := w.listTeams(t, w.member, team.ListParams{})

	if len(page.Teams) != 1 {
		t.Fatalf("got %d teams, want 1", len(page.Teams))
	}

	got := page.Teams[0]
	if got.ID != made.ID || got.WorkspaceID != w.workspace {
		t.Errorf("team = %s in %s, want %s in %s", got.ID, got.WorkspaceID, made.ID, w.workspace)
	}
	if len(got.Statuses) != len(team.DefaultStatuses) {
		t.Fatalf("got %d statuses, want %d", len(got.Statuses), len(team.DefaultStatuses))
	}
	for i, st := range got.Statuses {
		want := made.Statuses[i]
		if st != want {
			t.Errorf("status %d = %+v, want %+v - the list and Create disagree", i, st, want)
		}
		if st.Category == "" || st.Position == 0 {
			t.Errorf("status %q has no category or position: %+v", st.Name, st)
		}
	}
}

// One statement serves the whole page, so the risk is a status landing on the wrong team.
func TestEachTeamInAPageGetsOnlyItsOwnStatusesInPositionOrder(t *testing.T) {
	w := newWorld(t)
	a := w.newTeam(t, "A")
	b := w.newTeam(t, "B")
	w.newTeam(t, "Bare") // gets three too; the point is the grouping

	// Reorder B's workflow so position order differs from insertion order.
	if _, err := w.pool.Exec(context.Background(),
		`UPDATE status SET position = 10 - position WHERE team_id = $1::uuid`, b.ID); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	page := w.listTeams(t, w.member, team.ListParams{})

	byID := map[string]team.Team{}
	for _, tm := range page.Teams {
		byID[tm.ID] = tm
	}

	for _, tm := range []team.Team{a, b} {
		got := byID[tm.ID]
		if len(got.Statuses) != 3 {
			t.Fatalf("team %s has %d statuses, want 3", tm.Name, len(got.Statuses))
		}
		for _, st := range got.Statuses {
			var owner string
			if err := w.pool.QueryRow(context.Background(),
				`SELECT team_id::text FROM status WHERE id = $1::uuid`, st.ID).Scan(&owner); err != nil {
				t.Fatalf("owner of %s: %v", st.ID, err)
			}
			if owner != tm.ID {
				t.Errorf("status %s belongs to %s but arrived on %s", st.ID, owner, tm.ID)
			}
		}
		for i := 1; i < len(got.Statuses); i++ {
			if got.Statuses[i].Position < got.Statuses[i-1].Position {
				t.Errorf("team %s statuses are not in position order: %+v", tm.Name, got.Statuses)
			}
		}
	}

	// B was reversed, so its first status is the one that was last.
	if got := byID[b.ID].Statuses[0].Category; got != "done" {
		t.Errorf("reordered team starts with %q, want done", got)
	}
}

// A bare team has no statuses; it is still a team, and its list entry says so with an
// empty set rather than dropping the team.
func TestABareTeamIsListedWithNoStatuses(t *testing.T) {
	w := newWorld(t)

	if _, err := w.teams.Create(context.Background(), w.member,
		team.CreateParams{WorkspaceID: w.workspace, Name: "Imported", Bare: true}); err != nil {
		t.Fatalf("create bare: %v", err)
	}

	page := w.listTeams(t, w.member, team.ListParams{})
	if len(page.Teams) != 1 || len(page.Teams[0].Statuses) != 0 {
		t.Errorf("got %+v, want one team with no statuses", page.Teams)
	}
}

// Workspace isolation, both directions: scope is not a filter condition and no body
// reaches across it.
func TestTheTeamListShowsOnlyTheCallersWorkspace(t *testing.T) {
	w := newWorld(t)
	mine := w.newTeam(t, "Mine")
	theirs := w.outsiderTeam(t, "Theirs")

	got := w.listTeams(t, w.member, team.ListParams{})
	if len(got.Teams) != 1 || got.Teams[0].ID != mine.ID {
		t.Errorf("member saw %v, want only %s", teamNames(got), mine.ID)
	}

	got = w.listTeams(t, w.outsid, team.ListParams{})
	if len(got.Teams) != 1 || got.Teams[0].ID != theirs.ID {
		t.Errorf("outsider saw %v, want only %s", teamNames(got), theirs.ID)
	}

	// And a filter that asks for the other workspace's team by name still cannot see it.
	got = w.listTeams(t, w.member, team.ListParams{Filter: nameIs(filter.Eq, "Theirs")})
	if len(got.Teams) != 0 {
		t.Errorf("a name filter reached across the workspace boundary: %v", teamNames(got))
	}
}

func TestAnAccountWithNoMembershipSeesNoTeams(t *testing.T) {
	w := newWorld(t)
	w.newTeam(t, "Mine")

	if _, err := w.pool.Exec(context.Background(),
		`DELETE FROM workspace_member WHERE account_id = $1::uuid`, w.member); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	if page := w.listTeams(t, w.member, team.ListParams{}); len(page.Teams) != 0 {
		t.Errorf("an account with nowhere to go saw %d teams", len(page.Teams))
	}
}

// With two memberships the list follows the one `me` reports - the oldest - and the
// other workspace's teams stay out. ADR 0004 promised the two could not disagree; this
// is the test that lets that sentence stay true for the list.
func TestWithTwoMembershipsTheListFollowsTheWorkspaceMeReports(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	home := w.newTeam(t, "Home")
	away := w.outsiderTeam(t, "Away")

	// The member joins the second workspace too. Dated explicitly: both inserts would
	// otherwise share a transaction-start timestamp or differ by microseconds, and a
	// test of "oldest" should not rest on a race.
	older := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-24 * time.Hour)

	set := func(workspace string, at time.Time) {
		t.Helper()
		if _, err := w.pool.Exec(ctx,
			`INSERT INTO workspace_member (workspace_id, account_id, role, created_at)
			 VALUES ($1::uuid, $2::uuid, 'member', $3)
			 ON CONFLICT (workspace_id, account_id) DO UPDATE SET created_at = $3`,
			workspace, w.member, at); err != nil {
			t.Fatalf("membership: %v", err)
		}
	}

	accounts := account.NewService(w.pool)

	for _, tc := range []struct {
		name         string
		homeAt       time.Time
		awayAt       time.Time
		wantTeam     string
		wantWorkpace string
	}{
		{"home is oldest", older, newer, home.ID, w.workspace},
		{"away is oldest", newer, older, away.ID, w.otherWorkspace(t)},
	} {
		set(w.workspace, tc.homeAt)
		set(w.otherWorkspace(t), tc.awayAt)

		page := w.listTeams(t, w.member, team.ListParams{})
		if len(page.Teams) != 1 || page.Teams[0].ID != tc.wantTeam {
			t.Errorf("%s: listed %v, want only %s", tc.name, teamNames(page), tc.wantTeam)
		}

		profile, err := accounts.Profile(ctx, w.member)
		if err != nil {
			t.Fatalf("%s: profile: %v", tc.name, err)
		}
		if profile.WorkspaceID != tc.wantWorkpace || len(page.Teams) == 1 && page.Teams[0].WorkspaceID != profile.WorkspaceID {
			t.Errorf("%s: me says %s, the list is in %v", tc.name, profile.WorkspaceID, page.Teams)
		}
	}
}

func TestTheTeamListDefaultsToNameOrder(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		w.newTeam(t, name)
	}

	if got := teamNames(w.listTeams(t, w.member, team.ListParams{})); !equal(got, []string{"Alpha", "Bravo", "Charlie"}) {
		t.Errorf("got %v, want name order", got)
	}

	desc := w.listTeams(t, w.member, team.ListParams{Sort: []filter.Order{{Field: "name", Desc: true}}})
	if got := teamNames(desc); !equal(got, []string{"Charlie", "Bravo", "Alpha"}) {
		t.Errorf("descending got %v", got)
	}
}

func TestPaginationVisitsEveryTeamExactlyOnce(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		w.newTeam(t, name)
	}

	var (
		got   []string
		pages int
		p     = team.ListParams{Limit: 2}
	)
	for {
		page := w.listTeams(t, w.member, p)
		got = append(got, teamNames(page)...)
		pages++

		// Statuses must survive the page boundary, not just the first page.
		for _, tm := range page.Teams {
			if len(tm.Statuses) != 3 {
				t.Errorf("team %s on page %d has %d statuses", tm.Name, pages, len(tm.Statuses))
			}
		}

		if !page.HasMore {
			if page.NextCursor != "" {
				t.Error("the last page carries a cursor")
			}
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		p.Cursor = page.NextCursor
	}

	if pages != 3 || !equal(got, []string{"A", "B", "C", "D", "E"}) {
		t.Errorf("%d pages visiting %v, want 3 pages visiting A-E once each", pages, got)
	}
}

func TestExactlyPageSizeTeamsIsTheLastPage(t *testing.T) {
	w := newWorld(t)
	w.newTeam(t, "A")
	w.newTeam(t, "B")

	page := w.listTeams(t, w.member, team.ListParams{Limit: 2})
	if len(page.Teams) != 2 || page.HasMore || page.NextCursor != "" {
		t.Errorf("got %d teams, has_more %v, cursor %q", len(page.Teams), page.HasMore, page.NextCursor)
	}
}

func TestATeamFilterNarrowsTheList(t *testing.T) {
	w := newWorld(t)
	w.newTeam(t, "Platform")
	w.newTeam(t, "Growth")

	got := teamNames(w.listTeams(t, w.member, team.ListParams{Filter: nameIs(filter.Contains, "lat")}))
	if !equal(got, []string{"Platform"}) {
		t.Errorf("got %v, want Platform", got)
	}
}

// What the hydrating client uses for "changed since I last looked".
func TestTeamsCanBeFilteredByUpdatedAt(t *testing.T) {
	w := newWorld(t)
	old := w.newTeam(t, "Old")
	w.newTeam(t, "Fresh")

	if _, err := w.pool.Exec(context.Background(),
		`UPDATE team SET updated_at = now() - interval '2 days' WHERE id = $1::uuid`, old.ID); err != nil {
		t.Fatalf("age: %v", err)
	}

	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	page := w.listTeams(t, w.member, team.ListParams{Filter: filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "updated_at", Op: filter.Gt, Value: since}},
	}})

	if got := teamNames(page); !equal(got, []string{"Fresh"}) {
		t.Errorf("got %v, want only the recently updated team", got)
	}
}

func TestATeamCursorRefusesADifferentFilter(t *testing.T) {
	w := newWorld(t)
	w.newTeam(t, "A")
	w.newTeam(t, "B")

	page := w.listTeams(t, w.member, team.ListParams{Limit: 1})

	_, err := w.teams.List(context.Background(), w.member, team.ListParams{
		Limit: 1, Cursor: page.NextCursor, Filter: nameIs(filter.Eq, "A"),
	})
	if !errors.Is(err, filter.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

func TestTeamListFaultsAreLocatedInsideTheBody(t *testing.T) {
	w := newWorld(t)

	_, err := w.teams.List(context.Background(), w.member, team.ListParams{
		Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{{Field: "statuses", Op: filter.Eq, Value: "x"}}},
		Sort:   []filter.Order{{Field: "nonsense"}},
		Limit:  filter.MaxLimit + 1,
	})
	// The page size is checked first, and reported alone.
	var fe *filter.Error
	if !errors.As(err, &fe) || fe.Faults[0].Location != "body.limit" {
		t.Fatalf("err = %v, want a body.limit fault", err)
	}

	_, err = w.teams.List(context.Background(), w.member, team.ListParams{
		Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{{Field: "statuses", Op: filter.Eq, Value: "x"}}},
	})
	if !errors.As(err, &fe) || fe.Faults[0].Location != "body.filter.conditions[0].field" {
		t.Errorf("err = %v, want a located filter fault", err)
	}
}

// ─── projects ─────────────────────────────────────────────────────────────────

func TestTheProjectListShowsOnlyTheCallersWorkspace(t *testing.T) {
	w := newWorld(t)
	mine := w.newProject(t, w.newTeam(t, "Mine").ID, "Core")

	theirTeam := w.outsiderTeam(t, "Theirs")
	theirs, err := w.projects.Create(context.Background(), w.outsid,
		project.CreateParams{TeamID: theirTeam.ID, Name: "Secret"})
	if err != nil {
		t.Fatalf("create outsider project: %v", err)
	}

	got := w.listProjects(t, w.member, project.ListParams{})
	if ids := projectIDs(got); len(ids) != 1 || ids[0] != mine.ID {
		t.Errorf("member saw %v, want only %s", ids, mine.ID)
	}

	got = w.listProjects(t, w.outsid, project.ListParams{})
	if ids := projectIDs(got); len(ids) != 1 || ids[0] != theirs.ID {
		t.Errorf("outsider saw %v, want only %s", ids, theirs.ID)
	}

	// Asking for the other workspace's team by id does not reach it either.
	got = w.listProjects(t, w.member, project.ListParams{Filter: filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "team", Op: filter.Eq, Value: theirTeam.ID}},
	}})
	if len(got.Projects) != 0 {
		t.Errorf("a team filter reached across the workspace boundary: %v", projectIDs(got))
	}
}

func TestAnAccountWithNoMembershipSeesNoProjects(t *testing.T) {
	w := newWorld(t)
	w.newProject(t, w.newTeam(t, "Mine").ID, "Core")

	if _, err := w.pool.Exec(context.Background(),
		`DELETE FROM workspace_member WHERE account_id = $1::uuid`, w.member); err != nil {
		t.Fatalf("remove membership: %v", err)
	}

	if page := w.listProjects(t, w.member, project.ListParams{}); len(page.Projects) != 0 {
		t.Errorf("an account with nowhere to go saw %d projects", len(page.Projects))
	}
}

func TestWithTwoMembershipsTheProjectListFollowsTheWorkspaceMeReports(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	home := w.newProject(t, w.newTeam(t, "Home").ID, "Core")
	awayTeam := w.outsiderTeam(t, "Away")
	away, err := w.projects.Create(ctx, w.outsid, project.CreateParams{TeamID: awayTeam.ID, Name: "Core"})
	if err != nil {
		t.Fatalf("create away project: %v", err)
	}

	for _, tc := range []struct {
		name   string
		oldest string
		other  string
		want   string
	}{
		{"home is oldest", w.workspace, w.otherWorkspace(t), home.ID},
		{"away is oldest", w.otherWorkspace(t), w.workspace, away.ID},
	} {
		for workspace, at := range map[string]time.Time{
			tc.oldest: time.Now().Add(-48 * time.Hour),
			tc.other:  time.Now().Add(-24 * time.Hour),
		} {
			if _, err := w.pool.Exec(ctx,
				`INSERT INTO workspace_member (workspace_id, account_id, role, created_at)
				 VALUES ($1::uuid, $2::uuid, 'member', $3)
				 ON CONFLICT (workspace_id, account_id) DO UPDATE SET created_at = $3`,
				workspace, w.member, at); err != nil {
				t.Fatalf("membership: %v", err)
			}
		}

		if ids := projectIDs(w.listProjects(t, w.member, project.ListParams{})); len(ids) != 1 || ids[0] != tc.want {
			t.Errorf("%s: listed %v, want only %s", tc.name, ids, tc.want)
		}
	}
}

func TestTheProjectListCanBeFilteredByTeam(t *testing.T) {
	w := newWorld(t)
	a := w.newTeam(t, "A")
	b := w.newTeam(t, "B")
	inA := w.newProject(t, a.ID, "One")
	w.newProject(t, b.ID, "Two")

	page := w.listProjects(t, w.member, project.ListParams{Filter: filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "team", Op: filter.Eq, Value: a.ID}},
	}})

	if ids := projectIDs(page); len(ids) != 1 || ids[0] != inA.ID {
		t.Errorf("got %v, want only %s", ids, inA.ID)
	}
	if page.Projects[0].TeamID != a.ID {
		t.Errorf("team_id = %s, want %s", page.Projects[0].TeamID, a.ID)
	}
}

// The list is for choosing a project; the board is Get's job and Create's response.
func TestProjectListEntriesCarryNoBoard(t *testing.T) {
	w := newWorld(t)
	made := w.newProject(t, w.newTeam(t, "Mine").ID, "Core")
	if made.BoardID == "" {
		t.Fatal("setup: the project was not seeded a board")
	}

	got := w.listProjects(t, w.member, project.ListParams{}).Projects[0]
	if got.BoardID != "" || len(got.Columns) != 0 {
		t.Errorf("list entry carries a board: %+v", got)
	}
}

func TestTheProjectListDefaultsToNameOrder(t *testing.T) {
	w := newWorld(t)
	tm := w.newTeam(t, "Mine").ID
	for _, name := range []string{"Zeta", "Alpha", "Mid"} {
		w.newProject(t, tm, name)
	}

	page := w.listProjects(t, w.member, project.ListParams{})
	var got []string
	for _, p := range page.Projects {
		got = append(got, p.Name)
	}
	if !equal(got, []string{"Alpha", "Mid", "Zeta"}) {
		t.Errorf("got %v, want name order", got)
	}
}

// 0006 declines UNIQUE (team_id, name), so the sort key alone does not order these. The
// tiebreak does, and a cursor between two equal names must neither skip nor repeat.
func TestProjectsSharingANameArePagedThroughExactlyOnce(t *testing.T) {
	w := newWorld(t)
	tm := w.newTeam(t, "Mine").ID

	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		want[w.newProject(t, tm, "Core").ID] = true
	}

	var (
		seen  []string
		pages int
		p     = project.ListParams{Limit: 2}
	)
	for {
		page := w.listProjects(t, w.member, p)
		seen = append(seen, projectIDs(page)...)
		pages++

		if !page.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		p.Cursor = page.NextCursor
	}

	if pages != 3 || len(seen) != 5 {
		t.Fatalf("%d pages, %d ids, want 3 and 5: %v", pages, len(seen), seen)
	}
	for _, id := range seen {
		if !want[id] {
			t.Errorf("id %s came back twice or was never created", id)
		}
		delete(want, id)
	}
}

func TestExactlyPageSizeProjectsIsTheLastPage(t *testing.T) {
	w := newWorld(t)
	tm := w.newTeam(t, "Mine").ID
	w.newProject(t, tm, "A")
	w.newProject(t, tm, "B")

	page := w.listProjects(t, w.member, project.ListParams{Limit: 2})
	if len(page.Projects) != 2 || page.HasMore || page.NextCursor != "" {
		t.Errorf("got %d projects, has_more %v, cursor %q", len(page.Projects), page.HasMore, page.NextCursor)
	}
}

func TestAProjectCursorRefusesADifferentSort(t *testing.T) {
	w := newWorld(t)
	tm := w.newTeam(t, "Mine").ID
	w.newProject(t, tm, "A")
	w.newProject(t, tm, "B")

	page := w.listProjects(t, w.member, project.ListParams{Limit: 1})

	_, err := w.projects.List(context.Background(), w.member, project.ListParams{
		Limit: 1, Cursor: page.NextCursor, Sort: []filter.Order{{Field: "created_at"}},
	})
	if !errors.Is(err, filter.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

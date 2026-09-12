package migrate

import (
	"context"
	"testing"
)

// The constraints 0027_saved_view.sql claims. Three things carry most of the
// weight:
//
//   - the biconditional tying board_id to layout, both directions;
//   - that the biconditional forces CASCADE rather than SET NULL on board_id,
//     since nulling it would leave a row no CHECK can satisfy;
//   - the two composite references, which lapse exactly where the view makes
//     no claim - a team-wide view has no project to contradict.
func TestSavedViewConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// "sv-" namespaced per file - see schema_test.go.
	newTeamProject := func(t *testing.T, label string) (team, project string) {
		t.Helper()

		team = newTeam(t, pool, newWorkspace(t, pool, "sv-"+label), "Team sv-"+label)

		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return team, project
	}
	newProjectIn := func(t *testing.T, teamID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, $2) RETURNING id::text`,
			teamID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}

		return id
	}
	newBoard := func(t *testing.T, projectID, teamID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO board (project_id, team_id, name)
             VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, projectID, teamID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create board %s: %v", name, err)
		}

		return id
	}
	// A view with every optional column addressable. nil means leave it null.
	insert := func(teamID string, projectID, ownerID, boardID *string, name, layout string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO saved_view (team_id, project_id, owner_account_id, board_id, name, layout)
             VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6)`,
			teamID, projectID, ownerID, boardID, name, layout)
		return err
	}
	newListView := func(t *testing.T, teamID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, name, layout) VALUES ($1::uuid, $2, 'list')
             RETURNING id::text`, teamID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create list view %s: %v", name, err)
		}

		return id
	}
	countInTeam := func(t *testing.T, teamID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM saved_view WHERE team_id = $1::uuid`, teamID,
		).Scan(&n); err != nil {
			t.Fatalf("count views: %v", err)
		}

		return n
	}
	viewExists := func(t *testing.T, id string) bool {
		t.Helper()

		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM saved_view WHERE id = $1::uuid)`, id,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the view: %v", err)
		}

		return alive
	}
	leadsAnIndex := func(t *testing.T, column string) bool {
		t.Helper()

		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1 FROM pg_index i
                   JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'saved_view'::regclass AND a.attname = $1
             )`, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}

		return leads
	}

	// The sparsest legal view: shared with nobody in particular, scoped to no
	// project, owned by no one. Master spec 3.7 makes team the only required
	// scope.
	t.Run("accepts a team-wide list view with no project, owner or board", func(t *testing.T) {
		team, _ := newTeamProject(t, "minimal")
		view := newListView(t, team, "Everything")

		var project, owner, board *string
		var config string
		var shared bool
		if err := pool.QueryRow(ctx,
			`SELECT project_id::text, owner_account_id::text, board_id::text,
                    config::text, is_shared
               FROM saved_view WHERE id = $1::uuid`, view,
		).Scan(&project, &owner, &board, &config, &shared); err != nil {
			t.Fatalf("read the view back: %v", err)
		}
		if project != nil || owner != nil || board != nil {
			t.Errorf("minimal view came back scoped: project=%v owner=%v board=%v, want all NULL",
				project, owner, board)
		}
		if config != "{}" {
			t.Errorf("config = %s, want an empty object", config)
		}
		if shared {
			t.Error("is_shared defaulted to true, want false - a new view is a personal draft")
		}
	})

	t.Run("rejects a view in a team that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", nil, nil, nil, "Orphan", "list")
		wantPgError(t, err, foreignKeyViolation, "a view with no team")
	})

	t.Run("rejects a null team, name or layout", func(t *testing.T) {
		team, _ := newTeamProject(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO saved_view (team_id, name, layout) VALUES (NULL, 'V', 'list')`)
		wantPgError(t, err, notNullViolation, "a null team_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO saved_view (team_id, name, layout) VALUES ($1::uuid, NULL, 'list')`, team)
		wantPgError(t, err, notNullViolation, "a null name")

		_, err = pool.Exec(ctx,
			`INSERT INTO saved_view (team_id, name, layout) VALUES ($1::uuid, 'V', NULL)`, team)
		wantPgError(t, err, notNullViolation, "a null layout")
	})

	// Closed set, for status.category's reason: every renderer branches on it.
	t.Run("rejects an unknown layout", func(t *testing.T) {
		team, _ := newTeamProject(t, "unknown-layout")

		for _, layout := range []string{"table", "List", "kanban", ""} {
			err := insert(team, nil, nil, nil, "V", layout)
			wantPgError(t, err, checkViolation, "layout = "+layout)
		}
	})

	// Structure only, copying document_body_is_object. Nothing validates what
	// is inside, which is the same trade setting.value makes.
	t.Run("rejects a config that is not an object", func(t *testing.T) {
		team, _ := newTeamProject(t, "config-shape")

		for _, config := range []string{`"a string"`, `42`, `[1,2,3]`, `true`, `null`} {
			_, err := pool.Exec(ctx,
				`INSERT INTO saved_view (team_id, name, layout, config)
                 VALUES ($1::uuid, 'V', 'list', $2::jsonb)`, team, config)
			wantPgError(t, err, checkViolation, "config = "+config)
		}

		// ...and an arbitrary object is accepted, unvalidated.
		if _, err := pool.Exec(ctx,
			`INSERT INTO saved_view (team_id, name, layout, config)
             VALUES ($1::uuid, 'V', 'list', '{"anything": {"at": ["all"]}}'::jsonb)`, team,
		); err != nil {
			t.Fatalf("an arbitrary config object: %v", err)
		}
	})

	// The biconditional, both directions. This is what the two-table split
	// with LAM-48 costs, and what makes board_id mean something.
	t.Run("rejects a board view with no board", func(t *testing.T) {
		team, _ := newTeamProject(t, "board-without-board")

		err := insert(team, nil, nil, nil, "Sprint", "board")
		wantPgError(t, err, checkViolation, "layout = board with no board_id")
	})

	t.Run("rejects a list view that names a board", func(t *testing.T) {
		team, project := newTeamProject(t, "list-with-board")
		board := newBoard(t, project, team, "Sprint board")

		err := insert(team, &project, nil, &board, "My list", "list")
		wantPgError(t, err, checkViolation, "layout = list with a board_id")
	})

	t.Run("accepts a board view naming a board in its own project", func(t *testing.T) {
		team, project := newTeamProject(t, "board-happy-path")
		board := newBoard(t, project, team, "Sprint board")

		if err := insert(team, &project, nil, &board, "Sprint", "board"); err != nil {
			t.Fatalf("a board view of its own project's board: %v", err)
		}
	})

	// project_id is nullable, so the project-side composite reference is
	// MATCH SIMPLE and lapses here - which is correct: a team-wide view makes
	// no claim about a project, so there is nothing to contradict. The
	// team-side reference still applies.
	t.Run("accepts a team-wide board view of any board in its team", func(t *testing.T) {
		team, apollo := newTeamProject(t, "team-wide-board")
		borealis := newProjectIn(t, team, "Borealis")
		board := newBoard(t, borealis, team, "Borealis board")

		if err := insert(team, nil, nil, &board, "Somebody's board", "board"); err != nil {
			t.Fatalf("a team-wide board view: %v", err)
		}
		_ = apollo
	})

	t.Run("rejects a project belonging to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "sv-cross-project")
		home := newTeam(t, pool, workspace, "Team sv-cross-home")
		away := newTeam(t, pool, workspace, "Team sv-cross-away")

		foreign := newProjectIn(t, away, "Apollo")

		err := insert(home, &foreign, nil, nil, "Someone else's project", "list")
		wantPgError(t, err, foreignKeyViolation, "a project in another team")
	})

	t.Run("rejects a board belonging to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "sv-cross-board")
		home := newTeam(t, pool, workspace, "Team sv-board-home")
		away := newTeam(t, pool, workspace, "Team sv-board-away")

		awayProject := newProjectIn(t, away, "Apollo")
		foreign := newBoard(t, awayProject, away, "Someone else's board")

		err := insert(home, nil, nil, &foreign, "Foreign board", "board")
		wantPgError(t, err, foreignKeyViolation, "a board in another team")
	})

	// The project-side board reference on its own: same team, so the
	// team-side reference is satisfied, and only the project hop objects.
	t.Run("rejects a board from another project of the same team", func(t *testing.T) {
		team, apollo := newTeamProject(t, "cross-project-board")
		borealis := newProjectIn(t, team, "Borealis")
		board := newBoard(t, borealis, team, "Borealis board")

		err := insert(team, &apollo, nil, &board, "Wrong project", "board")
		wantPgError(t, err, foreignKeyViolation, "a board from a sibling project")
	})

	// LAM-50 step 1: deleting a project widens the view rather than
	// destroying it. The column-list SET NULL is what makes that possible -
	// a bare one would try to null the NOT NULL team_id and fail.
	t.Run("deleting a project widens a list view to its team", func(t *testing.T) {
		team, project := newTeamProject(t, "project-set-null")

		var view string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, project_id, name, layout)
             VALUES ($1::uuid, $2::uuid, 'Project work', 'list') RETURNING id::text`, team, project,
		).Scan(&view); err != nil {
			t.Fatalf("create the view: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete the project: %v", err)
		}

		var gotProject, gotTeam *string
		if err := pool.QueryRow(ctx,
			`SELECT project_id::text, team_id::text FROM saved_view WHERE id = $1::uuid`, view,
		).Scan(&gotProject, &gotTeam); err != nil {
			t.Fatalf("read the view back: %v", err)
		}
		if gotProject != nil {
			t.Errorf("view still points at project %v after it was deleted", *gotProject)
		}
		if gotTeam == nil || *gotTeam != team {
			t.Errorf("view team_id = %v, want %s - the column-list SET NULL must leave it alone",
				gotTeam, team)
		}
	})

	// The consequence of the biconditional. SET NULL would leave a row whose
	// layout is 'board' and whose board_id is null, which no CHECK can
	// satisfy - so the board would simply be undeletable. CASCADE is the only
	// coherent action, and a board view whose board is gone is not a view of
	// anything.
	t.Run("deleting a board deletes the views of it", func(t *testing.T) {
		team, project := newTeamProject(t, "board-cascade")
		board := newBoard(t, project, team, "Sprint board")

		var view string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, project_id, board_id, name, layout)
             VALUES ($1::uuid, $2::uuid, $3::uuid, 'Sprint', 'board') RETURNING id::text`,
			team, project, board,
		).Scan(&view); err != nil {
			t.Fatalf("create the board view: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM board WHERE id = $1::uuid`, board); err != nil {
			t.Fatalf("delete the board - SET NULL here would make this fail: %v", err)
		}
		if viewExists(t, view) {
			t.Error("a board view survived its board")
		}
	})

	// Both delete paths meet: the project's CASCADE reaches board, which
	// cascades to the view, while SET NULL (project_id) fires against the
	// same row. Nothing should complain.
	t.Run("deleting a project takes its board views with it", func(t *testing.T) {
		team, project := newTeamProject(t, "project-board-cascade")
		board := newBoard(t, project, team, "Sprint board")

		var boardView string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, project_id, board_id, name, layout)
             VALUES ($1::uuid, $2::uuid, $3::uuid, 'Sprint', 'board') RETURNING id::text`,
			team, project, board,
		).Scan(&boardView); err != nil {
			t.Fatalf("create the board view: %v", err)
		}
		listView := newListView(t, team, "Everything")

		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete the project: %v", err)
		}

		if viewExists(t, boardView) {
			t.Error("a board view survived the project its board belonged to")
		}
		if !viewExists(t, listView) {
			t.Error("deleting a project destroyed an unrelated team-wide list view")
		}
	})

	// LAM-24's call for comment.author_id, applied here: a shared team view
	// must survive its author leaving.
	t.Run("deleting the owner leaves a shared view standing and ownerless", func(t *testing.T) {
		team, _ := newTeamProject(t, "owner-set-null")
		owner := newAccount(t, pool, "sv-owner-set-null@example.com")

		var view string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, owner_account_id, name, layout, is_shared)
             VALUES ($1::uuid, $2::uuid, 'Team triage', 'list', true) RETURNING id::text`, team, owner,
		).Scan(&view); err != nil {
			t.Fatalf("create the view: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, owner); err != nil {
			t.Fatalf("delete the owner: %v", err)
		}

		if !viewExists(t, view) {
			t.Fatal("a shared team view was deleted with its author")
		}

		var gotOwner *string
		if err := pool.QueryRow(ctx,
			`SELECT owner_account_id::text FROM saved_view WHERE id = $1::uuid`, view,
		).Scan(&gotOwner); err != nil {
			t.Fatalf("read the owner back: %v", err)
		}
		if gotOwner != nil {
			t.Errorf("view still points at owner %v after the account was deleted", *gotOwner)
		}
	})

	// The rough edge the migration names rather than fixes. A private view
	// whose owner is deleted is visible to nobody and owned by nobody. The
	// database cannot repair it - the fix is "flip is_shared", and a foreign
	// key action cannot set a second column. Asserted so it is a known state
	// that whatever handles account deletion has to deal with, rather than
	// something discovered in production.
	t.Run("leaves a private view orphaned when its owner is deleted", func(t *testing.T) {
		team, _ := newTeamProject(t, "orphaned-private")
		owner := newAccount(t, pool, "sv-orphaned-private@example.com")

		var view string
		if err := pool.QueryRow(ctx,
			`INSERT INTO saved_view (team_id, owner_account_id, name, layout, is_shared)
             VALUES ($1::uuid, $2::uuid, 'My drafts', 'list', false) RETURNING id::text`, team, owner,
		).Scan(&view); err != nil {
			t.Fatalf("create the view: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, owner); err != nil {
			t.Fatalf("delete the owner: %v", err)
		}

		var gotOwner *string
		var shared bool
		if err := pool.QueryRow(ctx,
			`SELECT owner_account_id::text, is_shared FROM saved_view WHERE id = $1::uuid`, view,
		).Scan(&gotOwner, &shared); err != nil {
			t.Fatalf("read the view back: %v", err)
		}
		if gotOwner != nil || shared {
			t.Errorf("owner=%v is_shared=%v; this test pins the known gap - if the schema "+
				"now repairs orphaned private views, 0027's why block needs updating",
				gotOwner, shared)
		}
	})

	t.Run("deleting a team deletes its views", func(t *testing.T) {
		team, _ := newTeamProject(t, "team-cascade")
		newListView(t, team, "Everything")

		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete the team: %v", err)
		}
		if got := countInTeam(t, team); got != 0 {
			t.Errorf("%d views survived their team, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the view", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "sv-full-chain")
		team := newTeam(t, pool, workspace, "Team sv-full-chain")
		newListView(t, team, "Everything")

		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countInTeam(t, team); got != 0 {
			t.Errorf("%d views survived their workspace, want 0", got)
		}
	})

	// Every foreign key needs its index. team_id for the CASCADE and the
	// team's view list; the other three because SET NULL and CASCADE both
	// have to find referencing rows before they can act.
	for _, c := range []struct{ column, why string }{
		{"team_id", "the team's view list and the team-side CASCADE"},
		{"project_id", "the project-side SET NULL"},
		{"owner_account_id", "the owner-side SET NULL"},
		{"board_id", "the board-side CASCADE"},
	} {
		t.Run(c.column+" leads an index", func(t *testing.T) {
			if !leadsAnIndex(t, c.column) {
				t.Errorf("no index on saved_view leads with %s, so %s scans the table",
					c.column, c.why)
			}
		})
	}
}

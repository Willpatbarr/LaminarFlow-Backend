package migrate

import (
	"context"
	"testing"
)

// The constraints 0014_aspect_type.sql claims. The table is small, so most of
// this file asserts absences: names may repeat, any name is legal, and no row
// is privileged. Each of those is a decision LAM-21 makes explicitly, and each
// would be easy to undo by accident.
func TestAspectTypeConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	insert := func(teamID, name string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, $2)`,
			teamID, name)
		return err
	}
	countTypes := func(t *testing.T, teamID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM aspect_type WHERE team_id = $1::uuid`, teamID,
		).Scan(&n); err != nil {
			t.Fatalf("count aspect types: %v", err)
		}

		return n
	}

	// First, before anything below inserts: the migration itself must leave
	// the table empty. Catches an unconditional seed of the starter set.
	t.Run("the migration seeds nothing", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM aspect_type`).Scan(&n); err != nil {
			t.Fatalf("count aspect types: %v", err)
		}
		if n != 0 {
			t.Errorf("the migration left %d aspect types behind, want 0 - "+
				"a migration runs once, so it cannot seed the teams created after it", n)
		}
	})

	t.Run("rejects an aspect type in a team that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "Class")
		wantPgError(t, err, foreignKeyViolation, "a team that does not exist")
	})

	t.Run("rejects a null team and a null name", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "nulls"), "Platform")

		_, err := pool.Exec(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES (NULL, 'Class')`)
		wantPgError(t, err, notNullViolation, "a null team_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, NULL)`, team)
		wantPgError(t, err, notNullViolation, "a null name")
	})

	t.Run("accepts an aspect type with a team and a name", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "happy-path"), "Platform")

		if err := insert(team, "Class"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := countTypes(t, team); got != 1 {
			t.Errorf("team holds %d aspect types, want 1", got)
		}
	})

	// LAM-21 asks for no UNIQUE (team_id, name), matching status and project.
	// This locks the absence in so adding one later has to be deliberate.
	t.Run("allows one name twice in one team", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "duplicate-name"), "Platform")

		if err := insert(team, "Service"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(team, "Service"); err != nil {
			t.Fatalf("second insert with the same name: %v", err)
		}
		if got := countTypes(t, team); got != 2 {
			t.Errorf("team holds %d aspect types, want 2", got)
		}
	})

	// Scoping, from the other direction: two teams configuring their own
	// document shapes must not collide.
	t.Run("allows the same name in two teams", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "two-teams")
		platform := newTeam(t, pool, workspace, "Platform")
		design := newTeam(t, pool, workspace, "Design")

		if err := insert(platform, "Class"); err != nil {
			t.Fatalf("insert for the first team: %v", err)
		}
		if err := insert(design, "Class"); err != nil {
			t.Fatalf("insert the same name for a second team: %v", err)
		}
	})

	// The starter set is a suggestion, not a closed set. A CHECK constraining
	// name to the five spec examples would pass every other test in this file
	// and quietly ban a team from naming its own type.
	t.Run("accepts a name outside the starter set", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "own-name"), "Platform")

		for _, name := range []string{"Class", "Interface/Protocol", "Saga", "Anti-Pattern"} {
			if err := insert(team, name); err != nil {
				t.Fatalf("insert %q: %v", name, err)
			}
		}
	})

	t.Run("deleting a team deletes its aspect types", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "team-cascade"), "Platform")

		if err := insert(team, "Doomed"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}

		if got := countTypes(t, team); got != 0 {
			t.Errorf("%d aspect types survived their team, want 0", got)
		}
	})

	// workspace to team to aspect_type. No single table's own test covers a
	// break in another's.
	t.Run("deleting a workspace cascades all the way to the aspect type", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "full-chain-aspect")
		team := newTeam(t, pool, workspace, "Platform")

		if err := insert(team, "Doomed"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		if got := countTypes(t, team); got != 0 {
			t.Errorf("%d aspect types survived their workspace, want 0", got)
		}
	})

	// No UNIQUE constraint here means nothing else indexes team_id, so this
	// index is the only thing standing between a team lookup - or a team
	// delete - and a sequential scan. Reverse it to (name, team_id) and both
	// lose their index with no error anywhere else.
	t.Run("team_id leads an index", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid
                    AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'aspect_type'::regclass
                    AND a.attname = 'team_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on aspect_type leads with team_id, so the foreign key is unindexed")
		}
	})

	// Nothing creates an aspect type as a side effect of creating a team.
	//
	// Read what this does and does not catch. It catches a migration that
	// seeds unconditionally, and it catches a trigger or a default that fires
	// on team insert. It does NOT catch the shape LAM-21 step 3 actually
	// tempts you into - an INSERT ... SELECT over the team table in the
	// migration - because that runs once, against whatever teams exist at
	// migration time, and this team is created after. A seed like that leaves
	// this test passing while every team created from here on gets nothing.
	//
	// That is not a gap in the test so much as the whole argument against
	// seeding in a migration, stated as code: the failure mode is invisible
	// from inside the database. The why block in 0014_aspect_type.sql is the
	// real guard, and a reviewer is the thing enforcing it.
	t.Run("creating a team seeds no aspect types", func(t *testing.T) {
		team := newTeam(t, pool, newWorkspace(t, pool, "unseeded"), "Platform")

		if got := countTypes(t, team); got != 0 {
			t.Errorf("a new team already has %d aspect types, want 0 - "+
				"seeding belongs to team creation, not to a migration that runs once", got)
		}
	})
}

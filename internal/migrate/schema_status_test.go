package migrate

import (
	"context"
	"testing"
)

// The constraints 0010_status.sql claims, each asserted by trying to violate
// it. The interesting ones are the closed category set, and the two
// deliberate absences - position and name are both non-unique on purpose.
func TestStatusConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	insert := func(teamID, name string, position int, category string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO status (team_id, name, color, position, category)
             VALUES ($1::uuid, $2, '#3DAF62', $3, $4)`,
			teamID, name, position, category)
		return err
	}
	newStatusTeam := func(t *testing.T, label string) string {
		return newTeam(t, pool, newWorkspace(t, pool, label), "Platform")
	}

	t.Run("rejects a status in a team that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "To Do", 1, "not_started")
		wantPgError(t, err, foreignKeyViolation, "orphan status")
	})

	t.Run("rejects a null team", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO status (team_id, name, color, position, category)
             VALUES (NULL, 'To Do', '#3DAF62', 1, 'not_started')`)
		wantPgError(t, err, notNullViolation, "null team_id")
	})

	// The closed set is what makes reporting possible over user-named
	// statuses, so an unknown category has to be a hard error rather than a
	// status that silently vanishes from every burndown chart.
	t.Run("rejects an unknown category", func(t *testing.T) {
		team := newStatusTeam(t, "bad-category")

		wantPgError(t, insert(team, "Doing", 2, "inprogress"),
			checkViolation, "misspelled category")
		wantPgError(t, insert(team, "Doing", 2, ""),
			checkViolation, "empty category")
	})

	t.Run("accepts all three known categories", func(t *testing.T) {
		team := newStatusTeam(t, "good-categories")

		defaults := []struct {
			name     string
			position int
			category string
		}{
			{"To Do", 1, "not_started"},
			{"In Progress", 2, "in_progress"},
			{"Done", 3, "done"},
		}
		for _, d := range defaults {
			if err := insert(team, d.name, d.position, d.category); err != nil {
				t.Fatalf("insert %s: %v", d.name, err)
			}
		}
	})

	t.Run("rejects a null name, color, position or category", func(t *testing.T) {
		team := newStatusTeam(t, "null-columns")

		for _, c := range []struct {
			what string
			sql  string
		}{
			{"null name", `VALUES ($1::uuid, NULL, '#3DAF62', 1, 'done')`},
			{"null color", `VALUES ($1::uuid, 'Done', NULL, 1, 'done')`},
			{"null position", `VALUES ($1::uuid, 'Done', '#3DAF62', NULL, 'done')`},
			{"null category", `VALUES ($1::uuid, 'Done', '#3DAF62', 1, NULL)`},
		} {
			_, err := pool.Exec(ctx,
				`INSERT INTO status (team_id, name, color, position, category) `+c.sql, team)
			wantPgError(t, err, notNullViolation, c.what)
		}
	})

	// Deliberate absence. position is not unique per team so that reordering
	// is a plain UPDATE rather than a dance around a constraint. Adding
	// UNIQUE (team_id, position) later fails here, which is the point.
	t.Run("allows two statuses at one position", func(t *testing.T) {
		team := newStatusTeam(t, "shared-position")

		if err := insert(team, "Doing", 2, "in_progress"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(team, "Reviewing", 2, "in_progress"); err != nil {
			t.Fatalf("second insert at the same position: %v", err)
		}
	})

	// Deliberate absence, same reasoning as project: LAM-17 does not ask for
	// name uniqueness, so the absence is asserted rather than assumed.
	t.Run("allows one name twice in one team", func(t *testing.T) {
		team := newStatusTeam(t, "duplicate-name")

		if err := insert(team, "Blocked", 4, "in_progress"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert(team, "Blocked", 5, "in_progress"); err != nil {
			t.Fatalf("second insert with the same name: %v", err)
		}
	})

	// Statuses are per team, so two teams may run identically named boards
	// without colliding.
	t.Run("allows one name in two teams", func(t *testing.T) {
		ws := newWorkspace(t, pool, "two-boards")

		for _, name := range []string{"Platform", "Design"} {
			team := newTeam(t, pool, ws, name)
			if err := insert(team, "To Do", 1, "not_started"); err != nil {
				t.Fatalf("insert for team %s: %v", name, err)
			}
		}
	})

	t.Run("deleting a team deletes its statuses", func(t *testing.T) {
		team := newStatusTeam(t, "cascade")

		if err := insert(team, "Doomed", 1, "not_started"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}

		var statuses int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM status WHERE team_id = $1::uuid`, team,
		).Scan(&statuses); err != nil {
			t.Fatalf("count statuses: %v", err)
		}
		if statuses != 0 {
			t.Errorf("%d statuses survived their team, want 0", statuses)
		}
	})

	// One index doing two jobs: team_id leads it, so it serves the foreign
	// key lookup LAM-17 step 2 asks for, and position follows, so a team's
	// statuses come back in board order without a sort.
	t.Run("team_id leads an index and position follows", func(t *testing.T) {
		var covers bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute lead
                     ON lead.attrelid = i.indrelid AND lead.attnum = i.indkey[0]
                   JOIN pg_attribute second
                     ON second.attrelid = i.indrelid AND second.attnum = i.indkey[1]
                  WHERE i.indrelid = 'status'::regclass
                    AND lead.attname = 'team_id'
                    AND second.attname = 'position'
             )`,
		).Scan(&covers); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !covers {
			t.Error("no index on status leads with team_id followed by position")
		}
	})
}

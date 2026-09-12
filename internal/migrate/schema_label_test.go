package migrate

import (
	"context"
	"testing"
)

// The constraints 0023_label.sql claims. The interesting one is
// UNIQUE (team_id, name), which is the only place in this epic a table takes
// a uniqueness constraint its siblings decline - so both halves are asserted:
// that duplicates within a team are rejected, and that the same name in two
// teams is fine.
func TestLabelConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// "lb-" namespaced per file: testPool clears document but not team, so
	// two files creating a team with the same name collide on team's
	// UNIQUE (workspace_id, name) only in the full run.
	newLabelTeam := func(t *testing.T, label string) string {
		t.Helper()
		return newTeam(t, pool, newWorkspace(t, pool, "lb-"+label), "Team lb-"+label)
	}
	insert := func(teamID, name, color string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO label (team_id, name, color) VALUES ($1::uuid, $2, $3)`,
			teamID, name, color)
		return err
	}
	countLabels := func(t *testing.T, teamID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM label WHERE team_id = $1::uuid`, teamID,
		).Scan(&n); err != nil {
			t.Fatalf("count labels: %v", err)
		}

		return n
	}
	leadsAnIndex := func(t *testing.T, column string) bool {
		t.Helper()

		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid
                    AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'label'::regclass
                    AND a.attname = $1
             )`, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}

		return leads
	}

	t.Run("rejects a label in a team that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "bug", "#d73a4a")
		wantPgError(t, err, foreignKeyViolation, "a label with no team")
	})

	t.Run("rejects a null team, name or color", func(t *testing.T) {
		team := newLabelTeam(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO label (team_id, name, color) VALUES (NULL, 'bug', '#d73a4a')`)
		wantPgError(t, err, notNullViolation, "a null team_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO label (team_id, name, color) VALUES ($1::uuid, NULL, '#d73a4a')`, team)
		wantPgError(t, err, notNullViolation, "a null name")

		_, err = pool.Exec(ctx,
			`INSERT INTO label (team_id, name, color) VALUES ($1::uuid, 'bug', NULL)`, team)
		wantPgError(t, err, notNullViolation, "a null color")
	})

	// The departure from project, status and aspect_type, and the reason this
	// table is allowed to depart: two "bug" chips with different ids would
	// split a filter's results in half with nothing reporting an error.
	t.Run("rejects the same label name twice in one team", func(t *testing.T) {
		team := newLabelTeam(t, "duplicate-name")

		if err := insert(team, "bug", "#d73a4a"); err != nil {
			t.Fatalf("first label: %v", err)
		}

		// A different colour must not make it a different label.
		err := insert(team, "bug", "#ff0000")
		wantPgError(t, err, uniqueViolation, "the same label name twice in one team")

		if got := countLabels(t, team); got != 1 {
			t.Errorf("team holds %d labels, want 1", got)
		}
	})

	// ...and the constraint is scoped to the team, not global. Two teams
	// naming a label "bug" is the normal case, not a collision.
	t.Run("accepts the same label name in two teams", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "lb-shared-name")
		first := newTeam(t, pool, workspace, "Team lb-shared-first")
		second := newTeam(t, pool, workspace, "Team lb-shared-second")

		for _, team := range []string{first, second} {
			if err := insert(team, "bug", "#d73a4a"); err != nil {
				t.Fatalf("label 'bug' in team %s: %v", team, err)
			}
		}
	})

	// The declined CHECK on color, matching 0010_status.sql. Asserted so that
	// whatever ticket settles the palette knows it is adding a constraint
	// rather than fixing an oversight.
	t.Run("accepts any color format, having no CHECK", func(t *testing.T) {
		team := newLabelTeam(t, "color-formats")

		for _, color := range []string{"#d73a4a", "red", "palette.danger", "rgb(215,58,74)", ""} {
			if err := insert(team, "label "+color, color); err != nil {
				t.Fatalf("color %q: %v", color, err)
			}
		}
	})

	t.Run("deleting a team deletes its labels", func(t *testing.T) {
		team := newLabelTeam(t, "team-cascade")

		if err := insert(team, "bug", "#d73a4a"); err != nil {
			t.Fatalf("create label: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}
		if got := countLabels(t, team); got != 0 {
			t.Errorf("%d labels survived their team, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the label", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "lb-full-chain")
		team := newTeam(t, pool, workspace, "Team lb-full-chain")

		if err := insert(team, "bug", "#d73a4a"); err != nil {
			t.Fatalf("create label: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countLabels(t, team); got != 0 {
			t.Errorf("%d labels survived their workspace, want 0", got)
		}
	})

	// label carries no separate index on team_id: UNIQUE (team_id, name) is
	// the index, and it only is one because team_id leads. Reverse it to
	// (name, team_id) and the uniqueness claim is identical, every test above
	// still passes, and the foreign key plus its CASCADE plus the label
	// picker all start scanning the table.
	t.Run("team_id leads an index", func(t *testing.T) {
		if !leadsAnIndex(t, "team_id") {
			t.Error("no index on label leads with team_id, so the label picker " +
				"and the team-side CASCADE both scan the table")
		}
	})
}

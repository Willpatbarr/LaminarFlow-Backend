package migrate

import (
	"context"
	"testing"
)

// The constraints 0024_ticket_label.sql claims. The load-bearing one is the
// three-hop composite chain: a ticket must never carry a label belonging to
// another team, and there must be no combination of project_id and team_id
// that lets it happen. Most of this file attacks that from a different angle
// each time.
//
// The chain is longer than 0013's because the shared parent is team and it
// sits two levels above ticket:
//
//	(ticket_id, project_id) -> ticket  (id, project_id)
//	(project_id, team_id)   -> project (id, team_id)
//	(label_id,   team_id)   -> label   (id, team_id)
func TestTicketLabelConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// A team and a project under it, returned together because almost every
	// subtest needs both. "tl-" namespaced per file - see schema_test.go.
	newTeamProject := func(t *testing.T, label string) (team, project string) {
		t.Helper()

		team = newTeam(t, pool, newWorkspace(t, pool, "tl-"+label), "Team tl-"+label)

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
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, teamID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create project %s: %v", name, err)
		}

		return id
	}
	newLabel := func(t *testing.T, teamID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO label (team_id, name, color) VALUES ($1::uuid, $2, '#d73a4a')
             RETURNING id::text`, teamID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create label %s: %v", name, err)
		}

		return id
	}
	newTicket := func(t *testing.T, projectID, title string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, $2)
             RETURNING id::text`, projectID, title,
		).Scan(&id); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		return id
	}
	tag := func(ticketID, labelID, projectID, teamID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
             VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid)`,
			ticketID, labelID, projectID, teamID)
		return err
	}
	countOnTicket := func(t *testing.T, ticketID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ticket_label WHERE ticket_id = $1::uuid`, ticketID,
		).Scan(&n); err != nil {
			t.Fatalf("count labels on the ticket: %v", err)
		}

		return n
	}
	countWithLabel := func(t *testing.T, labelID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ticket_label WHERE label_id = $1::uuid`, labelID,
		).Scan(&n); err != nil {
			t.Fatalf("count tickets with the label: %v", err)
		}

		return n
	}
	ticketExists := func(t *testing.T, ticketID string) bool {
		t.Helper()

		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM ticket WHERE id = $1::uuid)`, ticketID,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the ticket: %v", err)
		}

		return alive
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
                  WHERE i.indrelid = 'ticket_label'::regclass
                    AND a.attname = $1
             )`, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}

		return leads
	}

	t.Run("accepts a ticket and a label in the same team", func(t *testing.T) {
		team, project := newTeamProject(t, "happy-path")
		ticket := newTicket(t, project, "Build the thing")

		if err := tag(ticket, newLabel(t, team, "bug"), project, team); err != nil {
			t.Fatalf("tag a ticket with its own team's label: %v", err)
		}
		if got := countOnTicket(t, ticket); got != 1 {
			t.Errorf("ticket carries %d labels, want 1", got)
		}
	})

	t.Run("rejects a ticket that does not exist", func(t *testing.T) {
		team, project := newTeamProject(t, "no-such-ticket")

		err := tag("00000000-0000-0000-0000-000000000000", newLabel(t, team, "bug"), project, team)
		wantPgError(t, err, foreignKeyViolation, "a ticket that does not exist")
	})

	t.Run("rejects a label that does not exist", func(t *testing.T) {
		team, project := newTeamProject(t, "no-such-label")
		ticket := newTicket(t, project, "Build the thing")

		err := tag(ticket, "00000000-0000-0000-0000-000000000000", project, team)
		wantPgError(t, err, foreignKeyViolation, "a label that does not exist")
	})

	// The reason this table carries two denormalised columns. Every value of
	// (project_id, team_id) is tried: there is no third option, because one
	// team_id cannot be both the project's team and the foreign label's.
	t.Run("rejects a label belonging to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tl-cross-team")
		home := newTeam(t, pool, workspace, "Team tl-cross-home")
		away := newTeam(t, pool, workspace, "Team tl-cross-away")

		project := newProjectIn(t, home, "Apollo")
		ticket := newTicket(t, project, "Build the thing")
		foreign := newLabel(t, away, "bug")

		// Naming the ticket's own team satisfies the ticket and project hops
		// and breaks the label hop.
		wantPgError(t, tag(ticket, foreign, project, home), foreignKeyViolation,
			"a foreign team's label, with the ticket's team")

		// Naming the label's team satisfies the label hop and breaks the
		// project hop instead.
		wantPgError(t, tag(ticket, foreign, project, away), foreignKeyViolation,
			"a foreign team's label, with the label's team")
	})

	// The middle hop on its own: a project_id that is real, and a team_id
	// that is real, but that do not belong together.
	t.Run("rejects a project and a team that are not parent and child", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tl-mismatched-middle")
		home := newTeam(t, pool, workspace, "Team tl-middle-home")
		other := newTeam(t, pool, workspace, "Team tl-middle-other")

		project := newProjectIn(t, home, "Apollo")
		ticket := newTicket(t, project, "Build the thing")

		// The label is in `other`, and so is the team_id - so the label hop
		// is satisfied and only the project hop objects.
		err := tag(ticket, newLabel(t, other, "bug"), project, other)
		wantPgError(t, err, foreignKeyViolation, "a project that does not belong to the named team")
	})

	// And the ticket hop on its own: the label, project and team all agree
	// with each other, but the ticket lives in a different project.
	t.Run("rejects a ticket that does not belong to the named project", func(t *testing.T) {
		team, project := newTeamProject(t, "mismatched-ticket")
		elsewhere := newProjectIn(t, team, "Borealis")

		ticket := newTicket(t, elsewhere, "Build the thing")

		err := tag(ticket, newLabel(t, team, "bug"), project, team)
		wantPgError(t, err, foreignKeyViolation, "a ticket from a different project in the same team")
	})

	t.Run("rejects a null in any of the four columns", func(t *testing.T) {
		team, project := newTeamProject(t, "nulls")
		ticket := newTicket(t, project, "Build the thing")
		label := newLabel(t, team, "bug")

		for _, c := range []struct {
			what  string
			query string
			args  []any
		}{
			{"a null ticket_id", `INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
                VALUES (NULL, $1::uuid, $2::uuid, $3::uuid)`, []any{label, project, team}},
			{"a null label_id", `INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
                VALUES ($1::uuid, NULL, $2::uuid, $3::uuid)`, []any{ticket, project, team}},
			{"a null project_id", `INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
                VALUES ($1::uuid, $2::uuid, NULL, $3::uuid)`, []any{ticket, label, team}},
			{"a null team_id", `INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
                VALUES ($1::uuid, $2::uuid, $3::uuid, NULL)`, []any{ticket, label, project}},
		} {
			_, err := pool.Exec(ctx, c.query, c.args...)
			wantPgError(t, err, notNullViolation, c.what)
		}
	})

	t.Run("rejects the same label on the same ticket twice", func(t *testing.T) {
		team, project := newTeamProject(t, "duplicate-pair")
		ticket := newTicket(t, project, "Build the thing")
		label := newLabel(t, team, "bug")

		if err := tag(ticket, label, project, team); err != nil {
			t.Fatalf("first tag: %v", err)
		}

		err := tag(ticket, label, project, team)
		wantPgError(t, err, uniqueViolation, "the same label on the same ticket twice")
	})

	// The many-to-many the table exists to express, in both directions.
	t.Run("accepts several labels on one ticket", func(t *testing.T) {
		team, project := newTeamProject(t, "many-labels")
		ticket := newTicket(t, project, "Build the thing")

		for _, name := range []string{"bug", "feature", "task"} {
			if err := tag(ticket, newLabel(t, team, name), project, team); err != nil {
				t.Fatalf("tag with %s: %v", name, err)
			}
		}
		if got := countOnTicket(t, ticket); got != 3 {
			t.Errorf("ticket carries %d labels, want 3", got)
		}
	})

	// The read the feature exists for: every ticket wearing one label, across
	// more than one project in the team.
	t.Run("accepts one label on tickets in several projects of its team", func(t *testing.T) {
		team, apollo := newTeamProject(t, "many-tickets")
		borealis := newProjectIn(t, team, "Borealis")
		label := newLabel(t, team, "bug")

		for _, project := range []string{apollo, borealis} {
			if err := tag(newTicket(t, project, "Build the thing"), label, project, team); err != nil {
				t.Fatalf("tag a ticket in project %s: %v", project, err)
			}
		}
		if got := countWithLabel(t, label); got != 2 {
			t.Errorf("label is on %d tickets, want 2", got)
		}
	})

	// Master spec 3.4's "no status is sacred", applied to labels: a label is
	// user-owned and deletable, and work must not pin it in place. The other
	// half is the one that matters more - deleting the label must not take
	// the work with it.
	t.Run("deleting a label removes it from tickets without deleting them", func(t *testing.T) {
		team, project := newTeamProject(t, "label-cascade")
		ticket := newTicket(t, project, "Survivor")
		label := newLabel(t, team, "bug")

		if err := tag(ticket, label, project, team); err != nil {
			t.Fatalf("tag: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM label WHERE id = $1::uuid`, label); err != nil {
			t.Fatalf("delete the label: %v", err)
		}

		if got := countOnTicket(t, ticket); got != 0 {
			t.Errorf("%d taggings survived their label, want 0", got)
		}
		if !ticketExists(t, ticket) {
			t.Error("deleting a label deleted the tickets wearing it")
		}
	})

	// ...and the mirror. Deleting a ticket must not take the label with it -
	// the label belongs to the team, not to the ticket.
	t.Run("deleting a ticket removes its taggings without deleting the label", func(t *testing.T) {
		team, project := newTeamProject(t, "ticket-cascade")
		ticket := newTicket(t, project, "Doomed")
		label := newLabel(t, team, "bug")

		if err := tag(ticket, label, project, team); err != nil {
			t.Fatalf("tag: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ticket WHERE id = $1::uuid`, ticket); err != nil {
			t.Fatalf("delete the ticket: %v", err)
		}

		if got := countWithLabel(t, label); got != 0 {
			t.Errorf("%d taggings survived their ticket, want 0", got)
		}

		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM label WHERE id = $1::uuid)`, label,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the label: %v", err)
		}
		if !alive {
			t.Error("deleting a ticket deleted the team's label")
		}
	})

	// Both legs at once: team owns the label directly and the ticket through
	// project. No single parent's own test covers the row at the bottom of
	// both.
	t.Run("deleting a team cascades down both legs to the tagging", func(t *testing.T) {
		team, project := newTeamProject(t, "team-cascade")
		ticket := newTicket(t, project, "Doomed")

		if err := tag(ticket, newLabel(t, team, "bug"), project, team); err != nil {
			t.Fatalf("tag: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete the team: %v", err)
		}
		if got := countOnTicket(t, ticket); got != 0 {
			t.Errorf("%d taggings survived their team, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the tagging", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tl-full-chain")
		team := newTeam(t, pool, workspace, "Team tl-full-chain")
		project := newProjectIn(t, team, "Apollo")
		ticket := newTicket(t, project, "Doomed")

		if err := tag(ticket, newLabel(t, team, "bug"), project, team); err != nil {
			t.Fatalf("tag: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countOnTicket(t, ticket); got != 0 {
			t.Errorf("%d taggings survived their workspace, want 0", got)
		}
	})

	// The first consequence of ON UPDATE NO ACTION, the same one 0013 carries.
	t.Run("blocks moving a labelled ticket to another project", func(t *testing.T) {
		team, home := newTeamProject(t, "move-ticket-blocked")
		away := newProjectIn(t, team, "Borealis")

		ticket := newTicket(t, home, "Build the thing")
		if err := tag(ticket, newLabel(t, team, "bug"), home, team); err != nil {
			t.Fatalf("tag: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket)
		wantPgError(t, err, foreignKeyViolation, "moving a labelled ticket between projects")
	})

	// The second, and the more surprising one - it is the middle hop that
	// produces it, and no other table in this schema has it. Correct all the
	// same: a project changing teams leaves every label on its tickets owned
	// by a team that no longer owns the work.
	t.Run("blocks moving a project with labelled tickets to another team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tl-move-project")
		home := newTeam(t, pool, workspace, "Team tl-move-home")
		away := newTeam(t, pool, workspace, "Team tl-move-away")

		project := newProjectIn(t, home, "Apollo")
		ticket := newTicket(t, project, "Build the thing")
		if err := tag(ticket, newLabel(t, home, "bug"), project, home); err != nil {
			t.Fatalf("tag: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE project SET team_id = $1::uuid WHERE id = $2::uuid`, away, project)
		wantPgError(t, err, foreignKeyViolation, "moving a project with labelled tickets between teams")
	})

	// ...and both blocks are scoped to the labelled case, not bans on moving.
	t.Run("allows moving an unlabelled ticket and an unlabelled project", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tl-move-allowed")
		home := newTeam(t, pool, workspace, "Team tl-allowed-home")
		away := newTeam(t, pool, workspace, "Team tl-allowed-away")

		project := newProjectIn(t, home, "Apollo")
		elsewhere := newProjectIn(t, home, "Borealis")
		ticket := newTicket(t, project, "Build the thing")

		if _, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, elsewhere, ticket,
		); err != nil {
			t.Fatalf("move an unlabelled ticket: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE project SET team_id = $1::uuid WHERE id = $2::uuid`, away, project,
		); err != nil {
			t.Fatalf("move a project with no labelled tickets: %v", err)
		}
	})

	// The read the feature exists for - "every ticket labelled bug" - plus
	// the label-side CASCADE.
	t.Run("label_id leads an index", func(t *testing.T) {
		if !leadsAnIndex(t, "label_id") {
			t.Error("no index on ticket_label leads with label_id, so filtering " +
				"by label and the label-side CASCADE both scan the table")
		}
	})

	// Deliberately no separate index on ticket_id: the composite primary key
	// is a btree led by it. This asserts the PK column order, which is what
	// makes the omission safe - reverse it to (label_id, ticket_id) and
	// rendering a ticket's chips loses its index with no error anywhere else.
	t.Run("ticket_id leads an index, via the primary key", func(t *testing.T) {
		if !leadsAnIndex(t, "ticket_id") {
			t.Error("no index on ticket_label leads with ticket_id, so the " +
				"primary key no longer covers the ticket-side foreign key")
		}
	})
}

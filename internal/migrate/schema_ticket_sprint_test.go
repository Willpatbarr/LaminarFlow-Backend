package migrate

import (
	"context"
	"testing"
)

// The constraints 0013_ticket_sprint.sql claims. The load-bearing one is the
// composite foreign key pair: a ticket and a sprint from different projects
// must not be pairable at all, and there must be no value of project_id that
// lets it happen. Most of this file attacks that from a different angle each
// time.
func TestTicketSprintConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// Each project needs its own workspace, so projects built for different
	// subtests cannot collide on team's UNIQUE (workspace_id, name).
	newProject := func(t *testing.T, label string) string {
		t.Helper()

		team := newTeam(t, pool, newWorkspace(t, pool, label), "Platform")

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&id); err != nil {
			t.Fatalf("create project: %v", err)
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
	newSprint := func(t *testing.T, projectID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO sprint (project_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, projectID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create sprint: %v", err)
		}

		return id
	}
	pair := func(ticketID, sprintID, projectID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO ticket_sprint (ticket_id, sprint_id, project_id)
             VALUES ($1::uuid, $2::uuid, $3::uuid)`,
			ticketID, sprintID, projectID)
		return err
	}
	countInSprint := func(t *testing.T, sprintID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ticket_sprint WHERE sprint_id = $1::uuid`, sprintID,
		).Scan(&n); err != nil {
			t.Fatalf("count associations: %v", err)
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
                  WHERE i.indrelid = 'ticket_sprint'::regclass
                    AND a.attname = $1
             )`, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}

		return leads
	}

	t.Run("accepts a ticket and a sprint in the same project", func(t *testing.T) {
		project := newProject(t, "happy-path")
		ticket := newTicket(t, project, "Build the thing")
		sprint := newSprint(t, project, "Sprint 1")

		if err := pair(ticket, sprint, project); err != nil {
			t.Fatalf("pair a ticket with a sprint in its own project: %v", err)
		}
		if got := countInSprint(t, sprint); got != 1 {
			t.Errorf("sprint holds %d tickets, want 1", got)
		}
	})

	t.Run("rejects a ticket that does not exist", func(t *testing.T) {
		project := newProject(t, "no-such-ticket")
		sprint := newSprint(t, project, "Sprint 1")

		err := pair("00000000-0000-0000-0000-000000000000", sprint, project)
		wantPgError(t, err, foreignKeyViolation, "a ticket that does not exist")
	})

	t.Run("rejects a sprint that does not exist", func(t *testing.T) {
		project := newProject(t, "no-such-sprint")
		ticket := newTicket(t, project, "Build the thing")

		err := pair(ticket, "00000000-0000-0000-0000-000000000000", project)
		wantPgError(t, err, foreignKeyViolation, "a sprint that does not exist")
	})

	// The reason this table carries project_id at all. Two plain foreign keys
	// would let both of these through.
	t.Run("rejects a ticket and a sprint from different projects", func(t *testing.T) {
		home := newProject(t, "cross-home")
		away := newProject(t, "cross-away")

		ticket := newTicket(t, home, "Build the thing")
		sprint := newSprint(t, away, "Someone else's sprint")

		// Naming the ticket's project satisfies the ticket side and breaks
		// the sprint side.
		wantPgError(t, pair(ticket, sprint, home), foreignKeyViolation,
			"a foreign sprint, with the ticket's project")

		// Naming the sprint's project breaks the other one. There is no third
		// option: one project_id cannot match two different projects.
		wantPgError(t, pair(ticket, sprint, away), foreignKeyViolation,
			"a foreign sprint, with the sprint's project")
	})

	t.Run("rejects a project_id belonging to neither parent", func(t *testing.T) {
		home := newProject(t, "third-project-home")
		elsewhere := newProject(t, "third-project-elsewhere")

		ticket := newTicket(t, home, "Build the thing")
		sprint := newSprint(t, home, "Sprint 1")

		err := pair(ticket, sprint, elsewhere)
		wantPgError(t, err, foreignKeyViolation, "a project_id neither parent carries")
	})

	t.Run("rejects a null ticket, sprint or project", func(t *testing.T) {
		project := newProject(t, "nulls")
		ticket := newTicket(t, project, "Build the thing")
		sprint := newSprint(t, project, "Sprint 1")

		_, err := pool.Exec(ctx,
			`INSERT INTO ticket_sprint (ticket_id, sprint_id, project_id)
             VALUES (NULL, $1::uuid, $2::uuid)`, sprint, project)
		wantPgError(t, err, notNullViolation, "a null ticket_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO ticket_sprint (ticket_id, sprint_id, project_id)
             VALUES ($1::uuid, NULL, $2::uuid)`, ticket, project)
		wantPgError(t, err, notNullViolation, "a null sprint_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO ticket_sprint (ticket_id, sprint_id, project_id)
             VALUES ($1::uuid, $2::uuid, NULL)`, ticket, sprint)
		wantPgError(t, err, notNullViolation, "a null project_id")
	})

	t.Run("rejects the same pair twice", func(t *testing.T) {
		project := newProject(t, "duplicate-pair")
		ticket := newTicket(t, project, "Build the thing")
		sprint := newSprint(t, project, "Sprint 1")

		if err := pair(ticket, sprint, project); err != nil {
			t.Fatalf("first pair: %v", err)
		}

		err := pair(ticket, sprint, project)
		wantPgError(t, err, uniqueViolation, "the same ticket in the same sprint twice")
	})

	// The many-to-many the table exists to express, in both directions.
	t.Run("accepts one ticket in two sprints", func(t *testing.T) {
		project := newProject(t, "ticket-two-sprints")
		ticket := newTicket(t, project, "Long-running work")

		for _, name := range []string{"Sprint 1", "Sprint 2"} {
			if err := pair(ticket, newSprint(t, project, name), project); err != nil {
				t.Fatalf("pair with %s: %v", name, err)
			}
		}
	})

	t.Run("accepts two tickets in one sprint", func(t *testing.T) {
		project := newProject(t, "sprint-two-tickets")
		sprint := newSprint(t, project, "Sprint 1")

		for _, title := range []string{"First", "Second"} {
			if err := pair(newTicket(t, project, title), sprint, project); err != nil {
				t.Fatalf("pair %s: %v", title, err)
			}
		}

		if got := countInSprint(t, sprint); got != 2 {
			t.Errorf("sprint holds %d tickets, want 2", got)
		}
	})

	t.Run("deleting a ticket deletes its associations", func(t *testing.T) {
		project := newProject(t, "ticket-cascade")
		ticket := newTicket(t, project, "Doomed")
		sprint := newSprint(t, project, "Sprint 1")

		if err := pair(ticket, sprint, project); err != nil {
			t.Fatalf("pair: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ticket WHERE id = $1::uuid`, ticket); err != nil {
			t.Fatalf("delete ticket: %v", err)
		}

		if got := countInSprint(t, sprint); got != 0 {
			t.Errorf("%d associations survived their ticket, want 0", got)
		}
	})

	t.Run("deleting a sprint deletes its associations, not its tickets", func(t *testing.T) {
		project := newProject(t, "sprint-cascade")
		ticket := newTicket(t, project, "Survivor")
		sprint := newSprint(t, project, "Doomed")

		if err := pair(ticket, sprint, project); err != nil {
			t.Fatalf("pair: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM sprint WHERE id = $1::uuid`, sprint); err != nil {
			t.Fatalf("delete sprint: %v", err)
		}

		if got := countInSprint(t, sprint); got != 0 {
			t.Errorf("%d associations survived their sprint, want 0", got)
		}

		// A timebox ending must not take the work with it.
		var alive bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM ticket WHERE id = $1::uuid)`, ticket,
		).Scan(&alive); err != nil {
			t.Fatalf("look for the ticket: %v", err)
		}
		if !alive {
			t.Error("deleting a sprint deleted the tickets in it")
		}
	})

	// workspace to team to project, then down both legs at once. No single
	// parent's own test covers the association at the bottom of both.
	t.Run("deleting a workspace cascades all the way to the association", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "full-chain-join")
		team := newTeam(t, pool, workspace, "Platform")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		sprint := newSprint(t, project, "Doomed")
		if err := pair(newTicket(t, project, "Doomed"), sprint, project); err != nil {
			t.Fatalf("pair: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		if got := countInSprint(t, sprint); got != 0 {
			t.Errorf("%d associations survived their workspace, want 0", got)
		}
	})

	// The consequence of ON UPDATE NO ACTION, asserted so it is a decision
	// rather than something a future reader discovers in production.
	t.Run("blocks moving a ticket to another project while it is in a sprint", func(t *testing.T) {
		home := newProject(t, "move-blocked-home")
		away := newProject(t, "move-blocked-away")

		ticket := newTicket(t, home, "Build the thing")
		if err := pair(ticket, newSprint(t, home, "Sprint 1"), home); err != nil {
			t.Fatalf("pair: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket)
		wantPgError(t, err, foreignKeyViolation, "moving a sprinted ticket between projects")
	})

	// ...and the block is scoped to that case, not a ban on moving tickets.
	t.Run("allows moving a ticket that is in no sprint", func(t *testing.T) {
		home := newProject(t, "move-allowed-home")
		away := newProject(t, "move-allowed-away")

		ticket := newTicket(t, home, "Build the thing")

		if _, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket,
		); err != nil {
			t.Fatalf("move an unsprinted ticket: %v", err)
		}
	})

	t.Run("sprint_id leads an index", func(t *testing.T) {
		if !leadsAnIndex(t, "sprint_id") {
			t.Error("no index on ticket_sprint leads with sprint_id, so " +
				"listing a sprint's tickets and the sprint-side CASCADE both scan the table")
		}
	})

	// Deliberately no separate index on ticket_id: the composite primary key
	// is a btree led by it. This asserts the PK column order, which is what
	// makes the omission safe - reverse it to (sprint_id, ticket_id) and the
	// ticket side loses its index with no error anywhere else.
	t.Run("ticket_id leads an index, via the primary key", func(t *testing.T) {
		if !leadsAnIndex(t, "ticket_id") {
			t.Error("no index on ticket_sprint leads with ticket_id, so the " +
				"primary key no longer covers the ticket-side foreign key")
		}
	})
}

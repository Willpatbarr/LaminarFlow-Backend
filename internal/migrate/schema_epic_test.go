package migrate

import (
	"context"
	"testing"
)

// The constraints 0022_epic.sql claims. Two of them are load-bearing and the
// rest of the file exists to hold them still:
//
//   - a ticket may never sit in another project's epic, from any angle;
//   - deleting an epic nulls ticket.epic_id and touches nothing else.
//
// The second is the one that would break quietly. The composite foreign key
// nulls every referencing column by default, and project_id is NOT NULL, so
// without the ON DELETE SET NULL (epic_id) column list an epic with tickets
// in it is simply undeletable.
func TestEpicConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// Fixture names are namespaced "ep-" per file: testPool clears document
	// but not team, so two files creating "Team platform" would collide on
	// team's UNIQUE (workspace_id, name) only in the full run.
	newProject := func(t *testing.T, label string) string {
		t.Helper()

		team := newTeam(t, pool, newWorkspace(t, pool, "ep-"+label), "Team ep-"+label)

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&id); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return id
	}
	newEpic := func(t *testing.T, projectID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO epic (project_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, projectID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create epic %s: %v", name, err)
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
	fileTicket := func(projectID, epicID, title string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, epic_id, title)
             VALUES ($1::uuid, $2::uuid, $3)`, projectID, epicID, title)
		return err
	}
	epicOf := func(t *testing.T, ticketID string) *string {
		t.Helper()

		var epic *string
		if err := pool.QueryRow(ctx,
			`SELECT epic_id::text FROM ticket WHERE id = $1::uuid`, ticketID,
		).Scan(&epic); err != nil {
			t.Fatalf("read the ticket's epic: %v", err)
		}

		return epic
	}
	countEpics := func(t *testing.T, projectID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM epic WHERE project_id = $1::uuid`, projectID,
		).Scan(&n); err != nil {
			t.Fatalf("count epics: %v", err)
		}

		return n
	}
	leadsAnIndex := func(t *testing.T, table, column string) bool {
		t.Helper()

		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid
                    AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = $1::regclass
                    AND a.attname = $2
             )`, table, column,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes on %s: %v", table, err)
		}

		return leads
	}

	t.Run("rejects an epic in a project that does not exist", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO epic (project_id, name)
             VALUES ('00000000-0000-0000-0000-000000000000'::uuid, 'Orphan')`)
		wantPgError(t, err, foreignKeyViolation, "an epic with no project")
	})

	t.Run("rejects a null project and a null name", func(t *testing.T) {
		project := newProject(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO epic (project_id, name) VALUES (NULL, 'Nameless project')`)
		wantPgError(t, err, notNullViolation, "a null project_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO epic (project_id, name) VALUES ($1::uuid, NULL)`, project)
		wantPgError(t, err, notNullViolation, "a null name")
	})

	// Following ticket.description, so nothing downstream has to decide
	// whether a null description differs from an empty one.
	t.Run("defaults description to empty rather than null", func(t *testing.T) {
		epic := newEpic(t, newProject(t, "default-description"), "Undescribed")

		var description *string
		if err := pool.QueryRow(ctx,
			`SELECT description FROM epic WHERE id = $1::uuid`, epic,
		).Scan(&description); err != nil {
			t.Fatalf("read the description: %v", err)
		}
		if description == nil {
			t.Fatal("description came back NULL, want an empty string")
		}
		if *description != "" {
			t.Errorf("description = %q, want empty", *description)
		}
	})

	// The declined UNIQUE (project_id, name), asserted so its absence reads
	// as a decision rather than an oversight. project, sprint and status all
	// decline the same constraint.
	t.Run("accepts two epics with the same name in one project", func(t *testing.T) {
		project := newProject(t, "duplicate-names")

		newEpic(t, project, "Checkout rework")
		newEpic(t, project, "Checkout rework")

		if got := countEpics(t, project); got != 2 {
			t.Errorf("project holds %d epics, want 2", got)
		}
	})

	// Master spec 3.2: the level is optional, so the sparsest ticket must
	// still be a legal ticket. MATCH SIMPLE is what makes this work - a null
	// epic_id switches the composite check off without consulting project_id.
	t.Run("accepts a ticket with no epic", func(t *testing.T) {
		project := newProject(t, "epicless")
		ticket := newTicket(t, project, "Unfiled work")

		if epic := epicOf(t, ticket); epic != nil {
			t.Errorf("a ticket created with no epic came back in epic %v, want NULL", *epic)
		}
	})

	t.Run("accepts a ticket in an epic in its own project", func(t *testing.T) {
		project := newProject(t, "happy-path")
		epic := newEpic(t, project, "Checkout rework")

		if err := fileTicket(project, epic, "Build the thing"); err != nil {
			t.Fatalf("file a ticket under an epic in its own project: %v", err)
		}
	})

	t.Run("rejects an epic_id naming nothing at all", func(t *testing.T) {
		project := newProject(t, "no-such-epic")

		err := fileTicket(project, "00000000-0000-0000-0000-000000000000", "Build the thing")
		wantPgError(t, err, foreignKeyViolation, "an epic that does not exist")
	})

	// The reason ticket_epic_fkey is composite. A plain epic_id reference
	// would let this through, and the symptom would surface much later as an
	// epic quietly listing another project's work.
	t.Run("rejects a ticket in another project's epic", func(t *testing.T) {
		home := newProject(t, "cross-home")
		away := newProject(t, "cross-away")

		foreign := newEpic(t, away, "Someone else's epic")

		// The ticket's own project_id is NOT NULL and is the one the foreign
		// key reads, so there is no second value to try: naming the foreign
		// epic from a home-project ticket cannot resolve.
		err := fileTicket(home, foreign, "Build the thing")
		wantPgError(t, err, foreignKeyViolation, "a ticket filed under a foreign project's epic")

		// And the same rejection arrives via UPDATE, which is the path a
		// drag-and-drop between boards would take.
		ticket := newTicket(t, home, "Build the thing")
		_, err = pool.Exec(ctx,
			`UPDATE ticket SET epic_id = $1::uuid WHERE id = $2::uuid`, foreign, ticket)
		wantPgError(t, err, foreignKeyViolation, "moving a ticket into a foreign project's epic")
	})

	// The load-bearing delete. CASCADE here would destroy work, and a bare
	// ON DELETE SET NULL would fail on project_id and make the epic
	// undeletable - so this asserts both halves: the ticket survives, and
	// only epic_id moved.
	t.Run("deleting an epic leaves its tickets standing and epicless", func(t *testing.T) {
		project := newProject(t, "epic-set-null")
		epic := newEpic(t, project, "Doomed")

		if err := fileTicket(project, epic, "Survivor"); err != nil {
			t.Fatalf("file the ticket: %v", err)
		}

		var ticket string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM ticket WHERE project_id = $1::uuid`, project,
		).Scan(&ticket); err != nil {
			t.Fatalf("find the ticket: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM epic WHERE id = $1::uuid`, epic); err != nil {
			t.Fatalf("delete the epic: %v", err)
		}

		if epic := epicOf(t, ticket); epic != nil {
			t.Errorf("ticket still points at epic %v after it was deleted", *epic)
		}

		// project_id is the other half of the composite foreign key, and the
		// column-list SET NULL exists to leave it alone.
		var project2 *string
		var title string
		if err := pool.QueryRow(ctx,
			`SELECT project_id::text, title FROM ticket WHERE id = $1::uuid`, ticket,
		).Scan(&project2, &title); err != nil {
			t.Fatalf("read the ticket back: %v", err)
		}
		if project2 == nil || *project2 != project {
			t.Errorf("ticket project_id = %v after deleting its epic, want %s", project2, project)
		}
		if title != "Survivor" {
			t.Errorf("ticket title = %q, want %q", title, "Survivor")
		}
	})

	t.Run("deleting a project deletes its epics", func(t *testing.T) {
		project := newProject(t, "project-cascade")
		newEpic(t, project, "Doomed")

		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete the project: %v", err)
		}
		if got := countEpics(t, project); got != 0 {
			t.Errorf("%d epics survived their project, want 0", got)
		}
	})

	// Both delete paths meet on the same rows here: the project's CASCADE
	// removes the tickets, and removing the epics fires SET NULL against
	// those same tickets. Nothing should deadlock or complain.
	t.Run("deleting a project removes epics and their tickets together", func(t *testing.T) {
		project := newProject(t, "cascade-collision")
		epic := newEpic(t, project, "Doomed")

		if err := fileTicket(project, epic, "Also doomed"); err != nil {
			t.Fatalf("file the ticket: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete the project: %v", err)
		}

		var tickets int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ticket WHERE project_id = $1::uuid`, project,
		).Scan(&tickets); err != nil {
			t.Fatalf("count tickets: %v", err)
		}
		if tickets != 0 {
			t.Errorf("%d tickets survived their project, want 0", tickets)
		}
		if got := countEpics(t, project); got != 0 {
			t.Errorf("%d epics survived their project, want 0", got)
		}
	})

	t.Run("deleting a workspace cascades all the way to the epic", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "ep-full-chain")
		team := newTeam(t, pool, workspace, "Team ep-full-chain")

		var project string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		newEpic(t, project, "Doomed")

		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countEpics(t, project); got != 0 {
			t.Errorf("%d epics survived their workspace, want 0", got)
		}
	})

	// The consequence of ON UPDATE NO ACTION on a composite foreign key, the
	// same one 0013 carries. Asserted so it is a decision rather than a
	// surprise someone meets in production.
	t.Run("blocks moving a ticket to another project while it is in an epic", func(t *testing.T) {
		home := newProject(t, "move-blocked-home")
		away := newProject(t, "move-blocked-away")

		if err := fileTicket(home, newEpic(t, home, "Checkout rework"), "Build the thing"); err != nil {
			t.Fatalf("file the ticket: %v", err)
		}

		var ticket string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM ticket WHERE project_id = $1::uuid`, home,
		).Scan(&ticket); err != nil {
			t.Fatalf("find the ticket: %v", err)
		}

		_, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket)
		wantPgError(t, err, foreignKeyViolation, "moving an epic'd ticket between projects")
	})

	// ...and the block is scoped to that case, not a ban on moving tickets.
	t.Run("allows moving a ticket that is in no epic", func(t *testing.T) {
		home := newProject(t, "move-allowed-home")
		away := newProject(t, "move-allowed-away")

		ticket := newTicket(t, home, "Unfiled work")

		if _, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket,
		); err != nil {
			t.Fatalf("move an epicless ticket: %v", err)
		}
	})

	// epic carries no separate index on project_id: UNIQUE (project_id, id)
	// is the index, and it only is one because project_id leads. Reverse the
	// constraint to (id, project_id) and the foreign key still resolves, the
	// migration still applies, every test above still passes, and deleting
	// one project starts scanning every epic in the instance. This is the
	// only thing standing between that and silence.
	t.Run("project_id leads an index on epic", func(t *testing.T) {
		if !leadsAnIndex(t, "epic", "project_id") {
			t.Error("no index on epic leads with project_id, so listing a " +
				"project's epics and the project-side CASCADE both scan the table")
		}
	})

	// Not for a read - for the SET NULL. Deleting an epic has to find every
	// ticket referencing it first.
	t.Run("epic_id leads an index on ticket", func(t *testing.T) {
		if !leadsAnIndex(t, "ticket", "epic_id") {
			t.Error("no index on ticket leads with epic_id, so deleting one " +
				"epic scans every ticket in the instance")
		}
	})
}

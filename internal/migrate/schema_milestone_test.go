package migrate

import (
	"context"
	"testing"
)

// The constraints 0033_milestone.sql claims, built on the pattern
// schema_epic_test.go asserts for epic:
//
//   - a ticket or an epic may never sit in another project's milestone;
//   - deleting a milestone nulls milestone_id on both and touches nothing else.
//
// LAM-82 decided only Milestone ships, so there is no release table to test.
func TestMilestoneConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// Fixture names are namespaced "ms-" per file, as epic's are "ep-".
	newProject := func(t *testing.T, label string) string {
		t.Helper()

		team := newTeam(t, pool, newWorkspace(t, pool, "ms-"+label), "Team ms-"+label)

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&id); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return id
	}
	newMilestone := func(t *testing.T, projectID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO milestone (project_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, projectID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create milestone %s: %v", name, err)
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
	newEpic := func(t *testing.T, projectID, name string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO epic (project_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, projectID, name,
		).Scan(&id); err != nil {
			t.Fatalf("create epic: %v", err)
		}

		return id
	}
	fileTicket := func(projectID, milestoneID, title string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, milestone_id, title)
             VALUES ($1::uuid, $2::uuid, $3)`, projectID, milestoneID, title)
		return err
	}
	fileEpic := func(projectID, milestoneID, name string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO epic (project_id, milestone_id, name)
             VALUES ($1::uuid, $2::uuid, $3)`, projectID, milestoneID, name)
		return err
	}
	milestoneOf := func(t *testing.T, table, id string) *string {
		t.Helper()

		var milestone *string
		if err := pool.QueryRow(ctx,
			`SELECT milestone_id::text FROM `+table+` WHERE id = $1::uuid`, id,
		).Scan(&milestone); err != nil {
			t.Fatalf("read the %s's milestone: %v", table, err)
		}

		return milestone
	}
	count := func(t *testing.T, table, projectID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE project_id = $1::uuid`, projectID,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
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

	t.Run("rejects a milestone in a project that does not exist", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO milestone (project_id, name)
             VALUES ('00000000-0000-0000-0000-000000000000'::uuid, 'Orphan')`)
		wantPgError(t, err, foreignKeyViolation, "a milestone with no project")
	})

	t.Run("rejects a null project and a null name", func(t *testing.T) {
		project := newProject(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO milestone (project_id, name) VALUES (NULL, 'Nameless project')`)
		wantPgError(t, err, notNullViolation, "a null project_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO milestone (project_id, name) VALUES ($1::uuid, NULL)`, project)
		wantPgError(t, err, notNullViolation, "a null name")
	})

	t.Run("defaults description to empty rather than null", func(t *testing.T) {
		milestone := newMilestone(t, newProject(t, "default-description"), "Undescribed")

		var description *string
		if err := pool.QueryRow(ctx,
			`SELECT description FROM milestone WHERE id = $1::uuid`, milestone,
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

	t.Run("accepts two milestones with the same name in one project", func(t *testing.T) {
		project := newProject(t, "duplicate-names")

		newMilestone(t, project, "v1.0")
		newMilestone(t, project, "v1.0")

		if got := count(t, "milestone", project); got != 2 {
			t.Errorf("project holds %d milestones, want 2", got)
		}
	})

	// Master spec 3.2: the level is optional. MATCH SIMPLE switches the
	// composite check off when milestone_id is null.
	t.Run("accepts a ticket and an epic with no milestone", func(t *testing.T) {
		project := newProject(t, "milestoneless")

		if m := milestoneOf(t, "ticket", newTicket(t, project, "Unfiled work")); m != nil {
			t.Errorf("a ticket created with no milestone came back in %v, want NULL", *m)
		}
		if m := milestoneOf(t, "epic", newEpic(t, project, "Unfiled epic")); m != nil {
			t.Errorf("an epic created with no milestone came back in %v, want NULL", *m)
		}
	})

	t.Run("accepts a ticket and an epic in a milestone in their own project", func(t *testing.T) {
		project := newProject(t, "happy-path")
		milestone := newMilestone(t, project, "v1.0")

		if err := fileTicket(project, milestone, "Build the thing"); err != nil {
			t.Fatalf("file a ticket under a milestone in its own project: %v", err)
		}
		if err := fileEpic(project, milestone, "Checkout rework"); err != nil {
			t.Fatalf("file an epic under a milestone in its own project: %v", err)
		}
	})

	t.Run("rejects a milestone_id naming nothing at all", func(t *testing.T) {
		project := newProject(t, "no-such-milestone")
		nothing := "00000000-0000-0000-0000-000000000000"

		wantPgError(t, fileTicket(project, nothing, "Build the thing"),
			foreignKeyViolation, "a ticket in a milestone that does not exist")
		wantPgError(t, fileEpic(project, nothing, "Checkout rework"),
			foreignKeyViolation, "an epic in a milestone that does not exist")
	})

	// The reason both foreign keys are composite.
	t.Run("rejects a ticket or an epic in another project's milestone", func(t *testing.T) {
		apollo := newProject(t, "cross-apollo")
		borealis := newProject(t, "cross-borealis")
		foreign := newMilestone(t, borealis, "v1.0")

		wantPgError(t, fileTicket(apollo, foreign, "Leaky"),
			foreignKeyViolation, "a ticket in another project's milestone")
		wantPgError(t, fileEpic(apollo, foreign, "Leaky"),
			foreignKeyViolation, "an epic in another project's milestone")

		// From the other angle: re-pointing an existing row.
		ticket := newTicket(t, apollo, "Honest")
		_, err := pool.Exec(ctx,
			`UPDATE ticket SET milestone_id = $1::uuid WHERE id = $2::uuid`, foreign, ticket)
		wantPgError(t, err, foreignKeyViolation, "re-pointing a ticket at another project's milestone")

		epic := newEpic(t, apollo, "Honest")
		_, err = pool.Exec(ctx,
			`UPDATE epic SET milestone_id = $1::uuid WHERE id = $2::uuid`, foreign, epic)
		wantPgError(t, err, foreignKeyViolation, "re-pointing an epic at another project's milestone")
	})

	// The one that would break quietly: without the column list, SET NULL
	// also nulls the NOT NULL project_id and the delete raises 23502.
	t.Run("deleting a milestone nulls milestone_id and keeps the work", func(t *testing.T) {
		project := newProject(t, "set-null")
		milestone := newMilestone(t, project, "Doomed")

		if err := fileTicket(project, milestone, "Survivor"); err != nil {
			t.Fatalf("file the ticket: %v", err)
		}
		if err := fileEpic(project, milestone, "Survivor epic"); err != nil {
			t.Fatalf("file the epic: %v", err)
		}

		if _, err := pool.Exec(ctx,
			`DELETE FROM milestone WHERE id = $1::uuid`, milestone,
		); err != nil {
			t.Fatalf("delete a milestone that has work in it: %v", err)
		}

		if got := count(t, "ticket", project); got != 1 {
			t.Errorf("project holds %d tickets after the delete, want 1", got)
		}
		if got := count(t, "epic", project); got != 1 {
			t.Errorf("project holds %d epics after the delete, want 1", got)
		}

		var ticket, epic string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM ticket WHERE project_id = $1::uuid`, project).Scan(&ticket); err != nil {
			t.Fatalf("find the ticket: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM epic WHERE project_id = $1::uuid`, project).Scan(&epic); err != nil {
			t.Fatalf("find the epic: %v", err)
		}
		if m := milestoneOf(t, "ticket", ticket); m != nil {
			t.Errorf("ticket.milestone_id = %v after the delete, want NULL", *m)
		}
		if m := milestoneOf(t, "epic", epic); m != nil {
			t.Errorf("epic.milestone_id = %v after the delete, want NULL", *m)
		}
	})

	t.Run("deleting a project removes milestones, epics and tickets together", func(t *testing.T) {
		project := newProject(t, "cascade-collision")
		milestone := newMilestone(t, project, "Doomed")

		if err := fileTicket(project, milestone, "Also doomed"); err != nil {
			t.Fatalf("file the ticket: %v", err)
		}
		if err := fileEpic(project, milestone, "Also doomed epic"); err != nil {
			t.Fatalf("file the epic: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete the project: %v", err)
		}

		for _, table := range []string{"milestone", "epic", "ticket"} {
			if got := count(t, table, project); got != 0 {
				t.Errorf("%d rows survived in %s after their project, want 0", got, table)
			}
		}
	})

	// ON UPDATE NO ACTION, as for epic: a ticket in a milestone cannot move
	// projects; one in no milestone can.
	t.Run("blocks moving a ticket to another project while it is in a milestone", func(t *testing.T) {
		home := newProject(t, "move-blocked-home")
		away := newProject(t, "move-blocked-away")

		if err := fileTicket(home, newMilestone(t, home, "v1.0"), "Build the thing"); err != nil {
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
		wantPgError(t, err, foreignKeyViolation, "moving a milestoned ticket between projects")
	})

	t.Run("allows moving a ticket that is in no milestone", func(t *testing.T) {
		home := newProject(t, "move-allowed-home")
		away := newProject(t, "move-allowed-away")

		ticket := newTicket(t, home, "Unfiled work")

		if _, err := pool.Exec(ctx,
			`UPDATE ticket SET project_id = $1::uuid WHERE id = $2::uuid`, away, ticket,
		); err != nil {
			t.Fatalf("move a milestoneless ticket: %v", err)
		}
	})

	// project_id must lead an index on milestone, or the project-side CASCADE
	// and the per-project listing both scan the table. See schema_epic_test.go.
	t.Run("project_id leads an index on milestone", func(t *testing.T) {
		if !leadsAnIndex(t, "milestone", "project_id") {
			t.Error("no index on milestone leads with project_id, so listing a " +
				"project's milestones and the project-side CASCADE both scan the table")
		}
	})

	// Not for a read - for the SET NULLs.
	t.Run("milestone_id leads an index on ticket and on epic", func(t *testing.T) {
		for _, table := range []string{"ticket", "epic"} {
			if !leadsAnIndex(t, table, "milestone_id") {
				t.Errorf("no index on %s leads with milestone_id, so deleting one "+
					"milestone scans every %s in the instance", table, table)
			}
		}
	})
}

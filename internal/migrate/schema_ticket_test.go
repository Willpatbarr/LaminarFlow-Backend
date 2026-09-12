package migrate

import (
	"context"
	"testing"
)

// The constraints 0011_ticket.sql claims. ticket references three parents
// with three different ON DELETE actions, so most of this file is about
// proving each parent's deletion does the right different thing.
func TestTicketConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	// scaffold builds workspace -> team -> project and returns the project,
	// since a ticket needs the whole chain above it to exist.
	scaffold := func(t *testing.T, label string) (workspace, team, project string) {
		t.Helper()

		workspace = newWorkspace(t, pool, label)
		team = newTeam(t, pool, workspace, "Platform")

		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, team,
		).Scan(&project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		return workspace, team, project
	}
	newStatus := func(t *testing.T, teamID string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO status (team_id, name, color, position, category)
             VALUES ($1::uuid, 'Doing', '#3DAF62', 1, 'in_progress')
             RETURNING id::text`, teamID,
		).Scan(&id); err != nil {
			t.Fatalf("create status: %v", err)
		}

		return id
	}
	countTickets := func(t *testing.T, projectID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM ticket WHERE project_id = $1::uuid`, projectID,
		).Scan(&n); err != nil {
			t.Fatalf("count tickets: %v", err)
		}

		return n
	}

	t.Run("rejects a ticket in a project that does not exist", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Orphan')`,
			"00000000-0000-0000-0000-000000000000")
		wantPgError(t, err, foreignKeyViolation, "orphan ticket")
	})

	t.Run("rejects a null project and a null title", func(t *testing.T) {
		_, _, project := scaffold(t, "null-columns")

		_, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES (NULL, 'No project')`)
		wantPgError(t, err, notNullViolation, "null project_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, NULL)`, project)
		wantPgError(t, err, notNullViolation, "null title")
	})

	// LAM-18 step 4, adjusted: the epic and milestone columns are not here,
	// so the minimal ticket is a project and a title. No status, no assignee,
	// no description - the flexible hierarchy in its barest form.
	t.Run("accepts a ticket with only a project and a title", func(t *testing.T) {
		_, _, project := scaffold(t, "minimal")

		if _, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Minimal')`,
			project,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}

		var description string
		var status, assignee *string
		if err := pool.QueryRow(ctx,
			`SELECT description, status_id::text, assignee_account_id::text
               FROM ticket WHERE project_id = $1::uuid`, project,
		).Scan(&description, &status, &assignee); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if description != "" {
			t.Errorf("description defaulted to %q, want empty", description)
		}
		if status != nil || assignee != nil {
			t.Errorf("status %v and assignee %v, want both null", status, assignee)
		}
	})

	// The reason status_id is nullable. LAM-17 settled that no status is
	// sacred, so deleting one cannot be blocked - which means the ticket has
	// to survive it. CASCADE here would delete every ticket in the column.
	t.Run("deleting a status leaves the ticket standing, statusless", func(t *testing.T) {
		_, team, project := scaffold(t, "status-set-null")
		status := newStatus(t, team)

		if _, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, status_id, title)
             VALUES ($1::uuid, $2::uuid, 'Survivor')`, project, status,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM status WHERE id = $1::uuid`, status); err != nil {
			t.Fatalf("delete status: %v", err)
		}

		if got := countTickets(t, project); got != 1 {
			t.Fatalf("%d tickets survived the status, want 1", got)
		}

		var status2 *string
		if err := pool.QueryRow(ctx,
			`SELECT status_id::text FROM ticket WHERE project_id = $1::uuid`, project,
		).Scan(&status2); err != nil {
			t.Fatalf("read status_id: %v", err)
		}
		if status2 != nil {
			t.Errorf("status_id is %q, want null", *status2)
		}
	})

	// Work outlives the people who touched it. CASCADE here would delete
	// every ticket a departing account was assigned.
	t.Run("deleting an account leaves the ticket standing, unassigned", func(t *testing.T) {
		_, _, project := scaffold(t, "assignee-set-null")
		account := newAccount(t, pool, "leaver@example.com")

		if _, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, assignee_account_id, title)
             VALUES ($1::uuid, $2::uuid, 'Reassign me')`, project, account,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, account); err != nil {
			t.Fatalf("delete account: %v", err)
		}

		if got := countTickets(t, project); got != 1 {
			t.Fatalf("%d tickets survived the account, want 1", got)
		}

		var assignee *string
		if err := pool.QueryRow(ctx,
			`SELECT assignee_account_id::text FROM ticket WHERE project_id = $1::uuid`, project,
		).Scan(&assignee); err != nil {
			t.Fatalf("read assignee: %v", err)
		}
		if assignee != nil {
			t.Errorf("assignee_account_id is %q, want null", *assignee)
		}
	})

	t.Run("deleting a project deletes its tickets", func(t *testing.T) {
		_, _, project := scaffold(t, "project-cascade")

		if _, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Doomed')`, project,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
			t.Fatalf("delete project: %v", err)
		}

		if got := countTickets(t, project); got != 0 {
			t.Errorf("%d tickets survived their project, want 0", got)
		}
	})

	// The full chain: workspace to team to project to ticket, four levels of
	// CASCADE. No single table's own test covers a break in another's.
	t.Run("deleting a workspace cascades all the way to the ticket", func(t *testing.T) {
		workspace, _, project := scaffold(t, "full-chain")

		if _, err := pool.Exec(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Doomed')`, project,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM workspace WHERE id = $1::uuid`, workspace,
		); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}

		if got := countTickets(t, project); got != 0 {
			t.Errorf("%d tickets survived their workspace, want 0", got)
		}
	})

	// Every referencing column needs a leading index. project_id for the
	// board read, and the other two so a SET NULL does not scan the whole
	// ticket table to find the rows it has to null.
	for _, col := range []string{"project_id", "status_id", "assignee_account_id"} {
		t.Run(col+" leads an index", func(t *testing.T) {
			var leads bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (
                     SELECT 1
                       FROM pg_index i
                       JOIN pg_attribute a
                         ON a.attrelid = i.indrelid
                        AND a.attnum = i.indkey[0]
                      WHERE i.indrelid = 'ticket'::regclass
                        AND a.attname = $1
                 )`, col,
			).Scan(&leads); err != nil {
				t.Fatalf("read indexes: %v", err)
			}
			if !leads {
				t.Errorf("no index on ticket leads with %s", col)
			}
		})
	}
}

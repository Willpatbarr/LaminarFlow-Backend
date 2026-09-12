package document

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// LAM-27: the flexible hierarchy, proven at the database level.
//
// Master spec 3.2 requires that an item without its optional parents is a
// legitimate item rather than incomplete data. Every entity ticket in this
// epic left the optional parents nullable to honour that, and each table's
// own test covers its own columns.
//
// This file exists for the two things those tests cannot do.
//
// The first is composition. Every per-table test builds a full scaffold above
// the row it is testing, because it has to; none of them asks whether the
// sparsest legal graph actually holds together end to end. A schema can pass
// every isolated constraint test and still have no valid minimal state.
//
// The second is regression. A nullable column can be tightened to NOT NULL in
// one line of a later migration, and nothing in a per-table test would
// necessarily notice - the table's own tests would be updated alongside it,
// which is exactly how a flexibility requirement quietly dies.
// TestOptionalParentsStayOptional pins the whole set in one place, so
// tightening any of them has to argue with a test that names the spec.
//
// On the ticket's suggested tooling: LAM-27 links Testcontainers, and this
// uses the repo's existing dbtest instead. dbtest.Create already builds a
// throwaway database per run and applies the real migrations through the real
// runner, which is the same guarantee for less machinery and no Docker
// dependency. CI runs it through ./scripts/test.sh like everything else, so
// step 3 needs no new setup.
func TestMinimalHierarchyHoldsTogether(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	// The scaffold, and nothing beyond it. No status, no sprint, no aspect
	// type, no members, no settings.
	var team string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Team minimal')
         RETURNING id::text`, ws,
	).Scan(&team); err != nil {
		t.Fatalf("team with only a workspace and a name: %v", err)
	}

	var project string
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Minimal')
         RETURNING id::text`, team,
	).Scan(&project); err != nil {
		t.Fatalf("project with only a team and a name: %v", err)
	}

	// Scenario 1: a ticket with only project_id. No status, no assignee and
	// no epic. LAM-18 omitted epic_id entirely rather than leave a uuid
	// pointing at a table that did not exist; LAM-46 added it with a real
	// reference, and master spec 3.2 makes that level optional - so a ticket
	// filed under no epic is a complete ticket, not an incomplete one.
	// milestone/release is still absent, deferred to LAM-47.
	var ticket string
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Minimal ticket')
         RETURNING id::text`, project,
	).Scan(&ticket); err != nil {
		t.Fatalf("ticket with only a project and a title: %v", err)
	}

	var statusID, assignee, epicID *string
	if err := pool.QueryRow(ctx,
		`SELECT status_id::text, assignee_account_id::text, epic_id::text
           FROM ticket WHERE id = $1::uuid`, ticket,
	).Scan(&statusID, &assignee, &epicID); err != nil {
		t.Fatalf("read the ticket back: %v", err)
	}
	if statusID != nil || assignee != nil || epicID != nil {
		t.Errorf("minimal ticket came back with status=%v assignee=%v epic=%v, want all NULL",
			statusID, assignee, epicID)
	}

	// It must also be absent from ticket_sprint rather than in some default
	// sprint - a ticket belongs to a sprint temporally or not at all.
	var sprintLinks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket_sprint WHERE ticket_id = $1::uuid`, ticket,
	).Scan(&sprintLinks); err != nil {
		t.Fatalf("count sprint links: %v", err)
	}
	if sprintLinks != 0 {
		t.Errorf("minimal ticket is in %d sprints, want 0", sprintLinks)
	}

	// Scenario 2: a normal document with no aspect type. Also no project and
	// no team - workspace-level, the sparsest a document gets.
	var loose string
	if err := pool.QueryRow(ctx,
		`INSERT INTO document (workspace_id) VALUES ($1::uuid) RETURNING id::text`, ws,
	).Scan(&loose); err != nil {
		t.Fatalf("document with only a workspace: %v", err)
	}

	var docType, title string
	var aspectType, docProject, docTeam *string
	if err := pool.QueryRow(ctx,
		`SELECT type, title, aspect_type_id::text, project_id::text, team_id::text
           FROM document WHERE id = $1::uuid`, loose,
	).Scan(&docType, &title, &aspectType, &docProject, &docTeam); err != nil {
		t.Fatalf("read the document back: %v", err)
	}
	if docType != TypeNormal {
		t.Errorf("bare document type = %q, want %q", docType, TypeNormal)
	}
	if aspectType != nil || docProject != nil || docTeam != nil {
		t.Errorf("bare document came back scoped: aspect=%v project=%v team=%v, want all NULL",
			aspectType, docProject, docTeam)
	}
	if title != "" {
		t.Errorf("bare document title = %q, want empty", title)
	}

	// Scenario 3: a document scoped to a team with no project. The arc
	// permits one scope or none, and this is the case the master spec calls
	// out - team-level documentation that belongs to no particular project.
	var teamDoc string
	if err := pool.QueryRow(ctx,
		`INSERT INTO document (workspace_id, team_id, title)
         VALUES ($1::uuid, $2::uuid, 'Team handbook') RETURNING id::text`, ws, team,
	).Scan(&teamDoc); err != nil {
		t.Fatalf("document scoped to a team with no project: %v", err)
	}

	// Scenario 4: a plain comment - no review status, and therefore no
	// reviewers. LAM-25 makes the second follow from the first.
	var plain string
	if err := pool.QueryRow(ctx,
		`INSERT INTO comment (document_id, body) VALUES ($1::uuid, 'a remark')
         RETURNING id::text`, teamDoc,
	).Scan(&plain); err != nil {
		t.Fatalf("plain comment on a team document: %v", err)
	}

	var reviewStatus, author, field *string
	var regionStart *int
	if err := pool.QueryRow(ctx,
		`SELECT review_status, author_id::text, field_id::text, region_start
           FROM comment WHERE id = $1::uuid`, plain,
	).Scan(&reviewStatus, &author, &field, &regionStart); err != nil {
		t.Fatalf("read the comment back: %v", err)
	}
	if reviewStatus != nil || author != nil || field != nil || regionStart != nil {
		t.Errorf("plain comment came back with status=%v author=%v field=%v region=%v, want all NULL",
			reviewStatus, author, field, regionStart)
	}

	var reviewers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM comment_reviewer WHERE comment_id = $1::uuid`, plain,
	).Scan(&reviewers); err != nil {
		t.Fatalf("count reviewers: %v", err)
	}
	if reviewers != 0 {
		t.Errorf("plain comment has %d reviewers, want 0", reviewers)
	}

	// A comment on the minimal ticket too, so both arms of comment's target
	// arc are exercised against minimal parents.
	if _, err := pool.Exec(ctx,
		`INSERT INTO comment (ticket_id, body) VALUES ($1::uuid, 'on the ticket')`, ticket,
	); err != nil {
		t.Fatalf("plain comment on a minimal ticket: %v", err)
	}
}

// A team with no members, no statuses and no aspect types is a legitimate
// team. Nothing in the schema seeds those - LAM-43 owns the bootstrap that
// eventually will - so this is the state every team is created in today, and
// it must not be a broken one.
func TestAFreshTeamIsUsableWithNothingConfigured(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	var team string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Team unconfigured')
         RETURNING id::text`, ws,
	).Scan(&team); err != nil {
		t.Fatalf("create team: %v", err)
	}

	for _, check := range []struct {
		what  string
		query string
	}{
		{"statuses", `SELECT count(*) FROM status WHERE team_id = $1::uuid`},
		{"aspect types", `SELECT count(*) FROM aspect_type WHERE team_id = $1::uuid`},
		{"settings", `SELECT count(*) FROM setting WHERE team_id = $1::uuid`},
	} {
		var n int
		if err := pool.QueryRow(ctx, check.query, team).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", check.what, err)
		}
		if n != 0 {
			t.Errorf("a fresh team has %d %s, want 0 - nothing seeds them yet (LAM-43)", n, check.what)
		}
	}

	// And a project and ticket can still be created under it, with no status
	// to put the ticket in.
	var project string
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Unconfigured')
         RETURNING id::text`, team,
	).Scan(&project); err != nil {
		t.Fatalf("project under an unconfigured team: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Statusless')`, project,
	); err != nil {
		t.Fatalf("ticket in a team with no statuses: %v", err)
	}
}

// The regression guard. Each entry is a column the flexible hierarchy depends
// on being nullable, with the reason it is. Tightening any of them to
// NOT NULL fails here, naming the decision it would overturn.
//
// This deliberately does not assert the complete set of nullable columns in
// the schema - a new nullable column is not a problem, and asserting the
// whole set would fail on every unrelated addition. It asserts that these
// specific ones stay optional.
func TestOptionalParentsStayOptional(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	required := map[string]string{
		"ticket.status_id":              "LAM-17: no status is sacred, so deleting one must not be blocked - which means tickets must be able to hold none",
		"ticket.assignee_account_id":    "LAM-18: work outlives the people assigned to it",
		"ticket.epic_id":                "LAM-46: master spec 3.2 makes epic an optional level, and deleting an epic must leave its tickets standing",
		"document.project_id":           "LAM-23: a document may be workspace-level",
		"document.team_id":              "LAM-23: a document may be workspace-level",
		"document.aspect_type_id":       "LAM-23: a normal document has no aspect type",
		"comment.ticket_id":             "LAM-24: one arm of the target arc - a document comment leaves this null",
		"comment.document_id":           "LAM-24: the other arm - a ticket comment leaves this null",
		"comment.field_id":              "LAM-24: a comment need not be anchored to an aspect field",
		"comment.author_id":             "LAM-24: deleting an account keeps the discussion and loses the attribution",
		"comment.review_status":         "LAM-24: null is a plain comment, which is the common case",
		"comment.region_start":          "LAM-24: a comment need not highlight a region",
		"comment.region_end":            "LAM-24: a comment need not highlight a region",
		"sprint.start_date":             "LAM-19: a sprint can be named before its dates are settled",
		"sprint.end_date":               "LAM-19: a sprint can be named before its dates are settled",
		"setting.workspace_id":          "LAM-16: one arm of the scope arc",
		"setting.team_id":               "LAM-16: the other arm of the scope arc",
		"search_index.ticket_id":        "LAM-26: one arm of the source arc",
		"search_index.document_id":      "LAM-26: one arm of the source arc",
		"search_index.comment_id":       "LAM-26: one arm of the source arc",
		"search_index.project_id":       "LAM-26: a search row may be scoped no narrower than its workspace",
		"search_index.team_id":          "LAM-26: a search row may be scoped no narrower than its workspace",
		"search_index.parent_reference": "LAM-26: only comment rows have a parent to name",
		"api_token.expires_at":          "LAM-15: a token may not expire",
		"saved_view.project_id":         "LAM-50: a saved view may span the whole team, and deleting a project widens it rather than destroying it",
		"saved_view.owner_account_id":   "LAM-50: a shared team view must survive its author leaving",
	}

	// saved_view.board_id is deliberately NOT pinned here. It is nullable,
	// but not optional in this test's sense - saved_view_board_matches_layout
	// makes it required exactly when layout is 'board' and forbidden
	// otherwise, so it is a layout discriminant rather than an optional
	// parent in the master spec 3.2 hierarchy. Its own biconditional is
	// asserted in both directions by TestSavedViewConstraints.

	rows, err := pool.Query(ctx,
		`SELECT table_name || '.' || column_name
           FROM information_schema.columns
          WHERE table_schema = 'public' AND is_nullable = 'NO'`)
	if err != nil {
		t.Fatalf("read column nullability: %v", err)
	}
	defer rows.Close()

	var tightened []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if reason, ok := required[column]; ok {
			tightened = append(tightened, fmt.Sprintf("  %s is NOT NULL, but %s", column, reason))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read column nullability: %v", err)
	}

	if len(tightened) > 0 {
		sort.Strings(tightened)
		t.Errorf("the flexible hierarchy has been tightened (master spec 3.2):\n%s",
			strings.Join(tightened, "\n"))
	}
}

// The columns pinned above must actually exist. Without this, renaming or
// dropping one would silently empty the guard rather than failing it.
func TestTheOptionalParentGuardNamesRealColumns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	for _, column := range []string{
		"ticket.status_id", "ticket.assignee_account_id", "ticket.epic_id",
		"document.project_id", "document.team_id", "document.aspect_type_id",
		"comment.ticket_id", "comment.document_id", "comment.field_id",
		"comment.author_id", "comment.review_status",
		"comment.region_start", "comment.region_end",
		"sprint.start_date", "sprint.end_date",
		"setting.workspace_id", "setting.team_id",
		"search_index.ticket_id", "search_index.document_id", "search_index.comment_id",
		"search_index.project_id", "search_index.team_id", "search_index.parent_reference",
		"api_token.expires_at",
		"saved_view.project_id", "saved_view.owner_account_id",
	} {
		table, name, _ := strings.Cut(column, ".")

		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1 FROM information_schema.columns
                  WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
             )`, table, name,
		).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", column, err)
		}
		if !exists {
			t.Errorf("%s is pinned by TestOptionalParentsStayOptional but does not exist - "+
				"the guard is silently passing on it", column)
		}
	}
}

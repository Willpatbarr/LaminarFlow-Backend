package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/jackc/pgx/v5/pgconn"
)

// LAM-75. What a ticket's text and its named ids are held to, at the service. The wire half
// (the 422 locations, the OpenAPI numbers) is internal/api's ticket_refs_test.go.

func (w world) one(t *testing.T, q string, args ...any) string {
	t.Helper()

	var id string
	if err := w.pool.QueryRow(context.Background(), q, args...).Scan(&id); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	return id
}

// statusIn makes a status in the team of the given project.
func (w world) statusIn(t *testing.T, projectID string) string {
	t.Helper()

	return w.one(t, `INSERT INTO status (team_id, name, color, position, category)
	                 SELECT p.team_id, 'Doing', '#fff', 1, 'in_progress' FROM project p
	                  WHERE p.id = $1::uuid RETURNING id::text`, projectID)
}

// otherTeamStatus is a status of a second team in the SAME workspace - the case a foreign
// key alone does not refuse, because the status exists.
func (w world) otherTeamStatus(t *testing.T) string {
	t.Helper()

	team := w.one(t, `INSERT INTO team (workspace_id, name)
	                  SELECT tm.workspace_id, 'Design' FROM project p JOIN team tm ON tm.id = p.team_id
	                   WHERE p.id = $1::uuid RETURNING id::text`, w.project)

	return w.one(t, `INSERT INTO status (team_id, name, color, position, category)
	                 VALUES ($1::uuid, 'Review', '#000', 1, 'in_progress') RETURNING id::text`, team)
}

func str(s string) *string { return &s }

const unknownID = "00000000-0000-4000-8000-000000000001"

func TestAnOverLongTitleIsRefusedAndNothingIsStored(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.Create(context.Background(), w.member, ticket.CreateParams{
		ProjectID: w.project, Title: strings.Repeat("a", ticket.MaxTitleLength+1),
	})

	var fe *textlimit.FieldError
	if !errors.As(err, &fe) || fe.Field != "title" || !errors.Is(err, textlimit.ErrTooLong) {
		t.Fatalf("err = %v, want a title ErrTooLong", err)
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 0 {
		t.Errorf("%d tickets stored by a refused create", n)
	}
}

func TestAnOverLongDescriptionIsRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", Description: strings.Repeat("d", ticket.MaxDescriptionLength+1),
	}); !errors.Is(err, textlimit.ErrTooLong) {
		t.Errorf("create: err = %v, want ErrTooLong", err)
	}

	// The limit itself is allowed.
	made, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", Description: strings.Repeat("d", ticket.MaxDescriptionLength),
	})
	if err != nil {
		t.Fatalf("create at the limit: %v", err)
	}

	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", Description: strings.Repeat("d", ticket.MaxDescriptionLength+1),
	}); !errors.Is(err, textlimit.ErrTooLong) {
		t.Errorf("update: err = %v, want ErrTooLong", err)
	}
}

func TestATitleOfOnlyWhitespaceIsBlank(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	for _, title := range []string{"", "   ", "\t\n", "  "} {
		if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{ProjectID: w.project, Title: title}); !errors.Is(err, textlimit.ErrBlank) {
			t.Errorf("create %q: err = %v, want ErrBlank", title, err)
		}
		if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: title}); !errors.Is(err, textlimit.ErrBlank) {
			t.Errorf("update %q: err = %v, want ErrBlank", title, err)
		}
	}
}

// Characters, not bytes: 200 three-byte characters is 600 bytes and is exactly at the limit.
func TestATitleOfMultiByteCharactersIsCountedInCharacters(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	at := strings.Repeat("あ", ticket.MaxTitleLength)
	made, err := w.svc.Create(ctx, w.member, ticket.CreateParams{ProjectID: w.project, Title: at})
	if err != nil {
		t.Fatalf("200 characters (%d bytes): %v", len(at), err)
	}
	if made.Title != at {
		t.Error("the title was altered on the way in")
	}

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: at + "あ"}); !errors.Is(err, textlimit.ErrTooLong) {
		t.Errorf("201 characters: err = %v, want ErrTooLong", err)
	}
}

// The limit is not in the database, so a ticket stored before it existed must stay readable.
func TestAnOverLongTitleAlreadyStoredStaysReadable(t *testing.T) {
	w := newWorld(t)
	long := strings.Repeat("x", 5000)
	id := w.one(t, `INSERT INTO ticket (project_id, title) VALUES ($1::uuid, $2) RETURNING id::text`, w.project, long)

	got, err := w.svc.Get(context.Background(), w.member, id)
	if err != nil || got.Title != long {
		t.Fatalf("get: %v (title %d chars)", err, len(got.Title))
	}
}

func (w world) rows(t *testing.T, q string, args ...any) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

func TestAnUnknownStatusIsRefusedOnCreateAndUpdate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", StatusID: str(unknownID)}); !errors.Is(err, ticket.ErrStatusNotFound) {
		t.Errorf("create: err = %v, want ErrStatusNotFound", err)
	}
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", StatusID: str(unknownID)}); !errors.Is(err, ticket.ErrStatusNotFound) {
		t.Errorf("update: err = %v, want ErrStatusNotFound", err)
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 1 {
		t.Errorf("%d tickets, want only the one from before", n)
	}
}

// ticket.status_id is a plain foreign key and ticket has no team_id, so Postgres accepts a
// status of another team. The service is the only thing that does not.
func TestAStatusOfAnotherTeamIsTheSameError(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	foreign := w.otherTeamStatus(t)
	own := w.statusIn(t, w.project)
	made := w.create(t, "Fix login")

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", StatusID: &foreign}); !errors.Is(err, ticket.ErrStatusNotFound) {
		t.Errorf("create with another team's status: err = %v, want ErrStatusNotFound", err)
	}
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", StatusID: &foreign}); !errors.Is(err, ticket.ErrStatusNotFound) {
		t.Errorf("update with another team's status: err = %v, want ErrStatusNotFound", err)
	}

	// And the team's own status still works, so the refusal above is about the team.
	got, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "t", StatusID: &own})
	if err != nil || got.StatusID == nil || *got.StatusID != own {
		t.Errorf("update with the team's own status: %v, %+v", err, got)
	}
	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", StatusID: &own}); err != nil {
		t.Errorf("create with the team's own status: %v", err)
	}
}

func TestAnUnknownAssigneeIsRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", AssigneeID: str(unknownID)}); !errors.Is(err, ticket.ErrAssigneeNotFound) {
		t.Errorf("create: err = %v, want ErrAssigneeNotFound", err)
	}
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", AssigneeID: str(unknownID)}); !errors.Is(err, ticket.ErrAssigneeNotFound) {
		t.Errorf("update: err = %v, want ErrAssigneeNotFound", err)
	}
}

// An account that exists, in a workspace that is not the ticket's, gets the very same
// answer as an id that does not exist at all.
func TestAnAssigneeOutsideTheWorkspaceIsTheSameError(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")
	colleague := w.colleague(t, "sarah")

	if _, err := w.svc.Create(ctx, w.member, ticket.CreateParams{
		ProjectID: w.project, Title: "t", AssigneeID: &w.outsid}); !errors.Is(err, ticket.ErrAssigneeNotFound) {
		t.Errorf("create: err = %v, want ErrAssigneeNotFound", err)
	}
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", AssigneeID: &w.outsid}); !errors.Is(err, ticket.ErrAssigneeNotFound) {
		t.Errorf("update: err = %v, want ErrAssigneeNotFound", err)
	}

	// A member of the workspace is fine, the caller included.
	for _, who := range []string{colleague, w.member} {
		got, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "t", AssigneeID: &who})
		if err != nil || got.AssigneeID == nil || *got.AssigneeID != who {
			t.Errorf("assign to a member: %v, %+v", err, got)
		}
	}
}

// Precedence: a ticket the caller cannot reach never learns whether an id it sent was
// valid, and a stale version is a conflict before it is a complaint about the status.
func TestRefusalPrecedenceOnUpdate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, w.outsid, ticket.UpdateParams{
		ID: made.ID, Title: "t", StatusID: str(unknownID), AssigneeID: str(unknownID)}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("unreachable ticket, bad ids: err = %v, want ErrNotFound", err)
	}

	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", StatusID: str(unknownID), IfVersion: version(7)}); asConflict(t, err).Current.Version != 1 {
		t.Error("stale version with a bad status was not a conflict")
	}

	// Status before assignee, one at a time.
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "t", StatusID: str(unknownID), AssigneeID: str(unknownID)}); !errors.Is(err, ticket.ErrStatusNotFound) {
		t.Errorf("both bad: err = %v, want ErrStatusNotFound first", err)
	}
}

// A project the caller cannot reach is not found, whatever else is wrong with the request.
func TestAnUnreachableProjectBeatsABadStatusOnCreate(t *testing.T) {
	w := newWorld(t)

	if _, err := w.svc.Create(context.Background(), w.outsid, ticket.CreateParams{
		ProjectID: w.project, Title: "t", StatusID: str(unknownID)}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Null for both is the normal ticket, and the checks must not trip on it.
func TestOmittingStatusAndAssigneeStillCreates(t *testing.T) {
	w := newWorld(t)

	got, err := w.svc.Create(context.Background(), w.member, ticket.CreateParams{ProjectID: w.project, Title: "t"})
	if err != nil || got.StatusID != nil || got.AssigneeID != nil {
		t.Errorf("create: %v, %+v", err, got)
	}
}

// The backstop is by constraint name. The in-statement checks make a real violation
// nearly impossible to provoke, so this holds the mapping and the names themselves.
func TestTheForeignKeysTheBackstopNamesExist(t *testing.T) {
	w := newWorld(t)

	for _, name := range []string{"ticket_status_id_fkey", "ticket_assignee_account_id_fkey"} {
		if n := w.rows(t, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = 'ticket'::regclass AND contype = 'f'`, name); n != 1 {
			t.Errorf("no foreign key named %s on ticket - ticket.refError maps it by that name", name)
		}
	}
}

func TestAForeignKeyViolationIsMappedByConstraintName(t *testing.T) {
	violation := func(code, constraint, message string) error {
		return fmt.Errorf("ticket: insert: %w", &pgconn.PgError{Code: code, ConstraintName: constraint, Message: message})
	}

	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"status":   {violation("23503", "ticket_status_id_fkey", ""), ticket.ErrStatusNotFound},
		"assignee": {violation("23503", "ticket_assignee_account_id_fkey", ""), ticket.ErrAssigneeNotFound},
		// The message is deliberately wrong: only the name decides.
		"name beats message": {violation("23503", "ticket_assignee_account_id_fkey", `violates "ticket_status_id_fkey"`), ticket.ErrAssigneeNotFound},
		"another key":        {violation("23503", "ticket_project_id_fkey", ""), nil},
		"another code":       {violation("23505", "ticket_status_id_fkey", ""), nil},
		"not a pg error":     {errors.New("boom"), nil},
		"no error":           {nil, nil},
	} {
		if got := ticket.RefError(tc.err); !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

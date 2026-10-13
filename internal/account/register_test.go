package account_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An install as registration finds it. withDefault is the state every real database is
// in after migrations: 0003 seeded a workspace called Default and nobody belongs to it.
// Without it, the install has no workspace at all.
func freshInstall(t *testing.T, withDefault bool) (*pgxpool.Pool, *account.Service) {
	t.Helper()

	w := newWorld(t)
	ctx := context.Background()

	for _, q := range []string{`DELETE FROM account`, `DELETE FROM workspace`} {
		if _, err := w.pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
	// Other tests in this package look the seeded workspace up by name, and this
	// database outlives the test.
	t.Cleanup(func() {
		_, _ = w.pool.Exec(ctx,
			`INSERT INTO workspace (name) SELECT 'Default'
			  WHERE NOT EXISTS (SELECT 1 FROM workspace WHERE name = 'Default')`)
	})

	if withDefault {
		if _, err := w.pool.Exec(ctx, `INSERT INTO workspace (name) VALUES ('Default')`); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	return w.pool, w.svc
}

func countOf(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

func newAccount(email string) account.NewAccount {
	return account.NewAccount{Email: email, PasswordHash: "not-a-real-hash", DisplayName: "Someone"}
}

// The branch the ticket names. A database with no workspace at all is not what a
// migrated install looks like, but it is what registration promises to handle.
func TestRegisterOnAnEmptyInstallCreatesTheWorkspaceAndMembership(t *testing.T) {
	pool, svc := freshInstall(t, false)

	reg, err := svc.Register(context.Background(), newAccount("first@example.com"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if !reg.WorkspaceCreated {
		t.Error("WorkspaceCreated = false on an install with no workspace")
	}
	if reg.Role != "owner" {
		t.Errorf("role = %q, want owner for the first member", reg.Role)
	}
	if n := countOf(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want exactly 1", n)
	}
	if n := countOf(t, pool,
		`SELECT count(*) FROM workspace_member WHERE workspace_id = $1::uuid AND account_id = $2::uuid`,
		reg.WorkspaceID, reg.AccountID); n != 1 {
		t.Errorf("%d membership rows for the new account, want 1", n)
	}
}

// The state of every real install: the workspace exists, from 0003, and is empty.
// Creating a second one here would leave the seeded workspace orphaned for good.
func TestRegisterAdoptsTheSeededWorkspaceAndOwnsIt(t *testing.T) {
	pool, svc := freshInstall(t, true)

	reg, err := svc.Register(context.Background(), newAccount("first@example.com"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if reg.WorkspaceCreated {
		t.Error("created a workspace although one existed")
	}
	if reg.Role != "owner" {
		t.Errorf("role = %q, want owner for the first member of the seeded workspace", reg.Role)
	}
	if n := countOf(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want the 1 that was seeded", n)
	}
}

func TestASecondRegistrationJoinsTheExistingWorkspaceAsMember(t *testing.T) {
	pool, svc := freshInstall(t, false)
	ctx := context.Background()

	first, err := svc.Register(ctx, newAccount("first@example.com"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := svc.Register(ctx, newAccount("second@example.com"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if second.WorkspaceID != first.WorkspaceID {
		t.Errorf("second account is in %s, want the first's %s", second.WorkspaceID, first.WorkspaceID)
	}
	if second.WorkspaceCreated {
		t.Error("the second registration created a workspace")
	}
	if second.Role != "member" {
		t.Errorf("role = %q, want member", second.Role)
	}
	if n := countOf(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want 1", n)
	}
	if n := countOf(t, pool, `SELECT count(*) FROM workspace_member WHERE workspace_id = $1::uuid`,
		first.WorkspaceID); n != 2 {
		t.Errorf("%d members, want 2", n)
	}
}

// 0007 compares on lower(email). A refusal also has to leave nothing behind: Register is
// one transaction, and an account row without its membership is the half-finished state
// it exists to rule out.
func TestADuplicateEmailIsRefusedWhateverItsCaseAndWritesNothing(t *testing.T) {
	pool, svc := freshInstall(t, true)
	ctx := context.Background()

	if _, err := svc.Register(ctx, newAccount("Will@Example.com")); err != nil {
		t.Fatalf("first: %v", err)
	}

	_, err := svc.Register(ctx, newAccount("  will@example.COM "))
	if !errors.Is(err, account.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}

	if n := countOf(t, pool, `SELECT count(*) FROM account`); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
	if n := countOf(t, pool, `SELECT count(*) FROM workspace_member`); n != 1 {
		t.Errorf("%d memberships, want 1", n)
	}
}

// Two first sign-ups at once. Without the advisory lock both see no workspace, both
// create one, and the install has two - each with an owner and no way to merge them.
func TestConcurrentFirstRegistrationsMakeOneWorkspace(t *testing.T) {
	pool, svc := freshInstall(t, false)

	const n = 8

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Register(context.Background(), newAccount("racer-"+string(rune('a'+i))+"@example.com"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("register: %v", err)
		}
	}

	if got := countOf(t, pool, `SELECT count(*) FROM workspace`); got != 1 {
		t.Errorf("%d workspaces after %d concurrent sign-ups, want 1", got, n)
	}
	if got := countOf(t, pool, `SELECT count(*) FROM workspace_member WHERE role = 'owner'`); got != 1 {
		t.Errorf("%d owners, want 1", got)
	}
	if got := countOf(t, pool, `SELECT count(*) FROM workspace_member`); got != n {
		t.Errorf("%d members, want %d", got, n)
	}
}

func TestProfileReportsTheAccountAndItsWorkspace(t *testing.T) {
	_, svc := freshInstall(t, true)
	ctx := context.Background()

	reg, err := svc.Register(ctx, account.NewAccount{
		Email: "Will@Example.com", PasswordHash: "h", DisplayName: "Will Barr",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := svc.Profile(ctx, reg.AccountID)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}

	want := account.Profile{Email: "Will@Example.com", DisplayName: "Will Barr", WorkspaceID: reg.WorkspaceID}
	if got != want {
		t.Errorf("profile = %+v, want %+v", got, want)
	}
}

func TestProfileOfAnAccountWithNoMembershipHasNoWorkspace(t *testing.T) {
	w := newWorld(t)

	var id string
	if err := w.pool.QueryRow(context.Background(),
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ('loner-' || gen_random_uuid() || '@x.com', 'h', 'Loner') RETURNING id::text`,
	).Scan(&id); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got, err := w.svc.Profile(context.Background(), id)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if got.WorkspaceID != "" {
		t.Errorf("workspace = %q, want none", got.WorkspaceID)
	}
}

func TestProfileOfAnUnknownAccountIsNotFound(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.Profile(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, account.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

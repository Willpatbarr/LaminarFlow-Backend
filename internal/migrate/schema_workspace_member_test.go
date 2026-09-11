package migrate

import (
	"context"
	"testing"
)

// The constraints 0020_workspace_member.sql claims.
//
// Back in internal/migrate with most of the epic's constraint tests: this
// table touches neither document nor search_index, so the boundary rule that
// pushed comment and document's tests into internal/document does not reach
// it.
func TestWorkspaceMemberConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	join := func(workspaceID, accountID, role string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO workspace_member (workspace_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, $3)`, workspaceID, accountID, role)
		return err
	}
	countMembers := func(t *testing.T, workspaceID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_member WHERE workspace_id = $1::uuid`, workspaceID,
		).Scan(&n); err != nil {
			t.Fatalf("count members: %v", err)
		}

		return n
	}

	t.Run("accepts a member at every known role", func(t *testing.T) {
		ws := newWorkspace(t, pool, "roles")

		for i, role := range []string{"owner", "admin", "member"} {
			account := newAccount(t, pool, "wm-role-"+role+"@example.com")
			if err := join(ws, account, role); err != nil {
				t.Fatalf("join as %s: %v", role, err)
			}
			if got := countMembers(t, ws); got != i+1 {
				t.Errorf("after %d joins the workspace has %d members", i+1, got)
			}
		}
	})

	t.Run("rejects an unknown role", func(t *testing.T) {
		ws := newWorkspace(t, pool, "bad-role")
		account := newAccount(t, pool, "wm-bad-role@example.com")

		err := join(ws, account, "superadmin")
		wantPgError(t, err, checkViolation, "a role outside the closed set")
	})

	t.Run("rejects a null role", func(t *testing.T) {
		ws := newWorkspace(t, pool, "null-role")
		account := newAccount(t, pool, "wm-null-role@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO workspace_member (workspace_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, NULL)`, ws, account)
		wantPgError(t, err, notNullViolation, "a membership with no role")
	})

	t.Run("rejects an account that does not exist", func(t *testing.T) {
		ws := newWorkspace(t, pool, "ghost-account")

		err := join(ws, "00000000-0000-0000-0000-000000000000", "member")
		wantPgError(t, err, foreignKeyViolation, "an account that does not exist")
	})

	t.Run("rejects a workspace that does not exist", func(t *testing.T) {
		account := newAccount(t, pool, "wm-ghost-workspace@example.com")

		err := join("00000000-0000-0000-0000-000000000000", account, "member")
		wantPgError(t, err, foreignKeyViolation, "a workspace that does not exist")
	})

	t.Run("rejects the same account joining twice", func(t *testing.T) {
		ws := newWorkspace(t, pool, "duplicate")
		account := newAccount(t, pool, "wm-duplicate@example.com")

		if err := join(ws, account, "member"); err != nil {
			t.Fatalf("first join: %v", err)
		}

		err := join(ws, account, "admin")
		wantPgError(t, err, uniqueViolation, "the same account joining a workspace twice")
	})

	t.Run("accepts one account in two workspaces", func(t *testing.T) {
		account := newAccount(t, pool, "wm-two-workspaces@example.com")

		for _, label := range []string{"two-a", "two-b"} {
			if err := join(newWorkspace(t, pool, label), account, "member"); err != nil {
				t.Fatalf("join %s: %v", label, err)
			}
		}
	})

	t.Run("deleting the workspace removes the membership", func(t *testing.T) {
		ws := newWorkspace(t, pool, "workspace-cascade")
		account := newAccount(t, pool, "wm-workspace-cascade@example.com")

		if err := join(ws, account, "owner"); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, ws); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countMembers(t, ws); got != 0 {
			t.Errorf("%d memberships survived their workspace, want 0", got)
		}
	})

	// The account is gone, so the membership is meaningless - unlike
	// comment.author_id, where authorship survives the account.
	t.Run("deleting the account removes the membership", func(t *testing.T) {
		ws := newWorkspace(t, pool, "account-cascade")
		account := newAccount(t, pool, "wm-account-cascade@example.com")

		if err := join(ws, account, "member"); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, account); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		if got := countMembers(t, ws); got != 0 {
			t.Errorf("%d memberships survived their account, want 0", got)
		}
	})

	// Absences. LAM-42 asks for neither, and both would be product rules.
	t.Run("allows a workspace with no members and none with an owner", func(t *testing.T) {
		ws := newWorkspace(t, pool, "ownerless")

		if got := countMembers(t, ws); got != 0 {
			t.Fatalf("a fresh workspace has %d members, want 0", got)
		}

		account := newAccount(t, pool, "wm-ownerless@example.com")
		if err := join(ws, account, "member"); err != nil {
			t.Fatalf("join a workspace that has no owner: %v", err)
		}
	})

	// LAM-42 step 5's decision, asserted. The composite PK leads with
	// workspace_id and cannot serve the login read.
	t.Run("account_id leads an index and workspace_id follows", func(t *testing.T) {
		if !memberIndexLeads(t, pool, "workspace_member", "account_id", "workspace_id") {
			t.Error("no index on workspace_member leads with account_id followed by workspace_id, " +
				"so \"which workspaces am I in\" scans the table")
		}
	})

	t.Run("workspace_id leads an index, via the primary key", func(t *testing.T) {
		if !memberIndexLeads(t, pool, "workspace_member", "workspace_id", "account_id") {
			t.Error("no index on workspace_member leads with workspace_id, so the primary key " +
				"no longer covers the workspace-side foreign key")
		}
	})
}

package migrate

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// memberIndexLeads reports whether any index on table has lead as its first
// column and next as its second.
func memberIndexLeads(t *testing.T, pool *pgxpool.Pool, table, lead, next string) bool {
	t.Helper()

	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (
             SELECT 1
               FROM pg_index i
               JOIN pg_attribute a
                 ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
               JOIN pg_attribute b
                 ON b.attrelid = i.indrelid AND b.attnum = i.indkey[1]
              WHERE i.indrelid = $1::regclass
                AND a.attname = $2
                AND b.attname = $3
         )`, table, lead, next,
	).Scan(&ok); err != nil {
		t.Fatalf("read indexes on %s: %v", table, err)
	}

	return ok
}

// The constraints 0021_team_member.sql claims.
//
// Most of this mirrors workspace_member, because the tables are the same
// shape. The part that does not mirror is the independence of the two levels,
// which is the whole reason LAM-42 built two tables and declined the
// composite-FK pattern - see TestMembershipLevelsAreIndependent below.
func TestTeamMemberConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	newTeamIn := func(t *testing.T, label string) string {
		t.Helper()
		return newTeam(t, pool, newWorkspace(t, pool, label), "Platform")
	}
	join := func(teamID, accountID, role string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO team_member (team_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, $3)`, teamID, accountID, role)
		return err
	}
	countMembers := func(t *testing.T, teamID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM team_member WHERE team_id = $1::uuid`, teamID,
		).Scan(&n); err != nil {
			t.Fatalf("count members: %v", err)
		}

		return n
	}

	t.Run("accepts a member at every known role", func(t *testing.T) {
		team := newTeamIn(t, "tm-roles")

		for _, role := range []string{"owner", "admin", "member"} {
			account := newAccount(t, pool, "tm-role-"+role+"@example.com")
			if err := join(team, account, role); err != nil {
				t.Fatalf("join as %s: %v", role, err)
			}
		}
		if got := countMembers(t, team); got != 3 {
			t.Errorf("team has %d members, want 3", got)
		}
	})

	t.Run("rejects an unknown role", func(t *testing.T) {
		team := newTeamIn(t, "tm-bad-role")
		account := newAccount(t, pool, "tm-bad-role@example.com")

		err := join(team, account, "reviewer")
		wantPgError(t, err, checkViolation, "a role outside the closed set")
	})

	t.Run("rejects a null role", func(t *testing.T) {
		team := newTeamIn(t, "tm-null-role")
		account := newAccount(t, pool, "tm-null-role@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO team_member (team_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, NULL)`, team, account)
		wantPgError(t, err, notNullViolation, "a membership with no role")
	})

	t.Run("rejects an account or team that does not exist", func(t *testing.T) {
		team := newTeamIn(t, "tm-ghost")
		account := newAccount(t, pool, "tm-ghost@example.com")
		missing := "00000000-0000-0000-0000-000000000000"

		wantPgError(t, join(team, missing, "member"),
			foreignKeyViolation, "an account that does not exist")
		wantPgError(t, join(missing, account, "member"),
			foreignKeyViolation, "a team that does not exist")
	})

	t.Run("rejects the same account joining twice", func(t *testing.T) {
		team := newTeamIn(t, "tm-duplicate")
		account := newAccount(t, pool, "tm-duplicate@example.com")

		if err := join(team, account, "member"); err != nil {
			t.Fatalf("first join: %v", err)
		}

		err := join(team, account, "admin")
		wantPgError(t, err, uniqueViolation, "the same account joining a team twice")
	})

	t.Run("deleting the team removes the membership", func(t *testing.T) {
		team := newTeamIn(t, "tm-team-cascade")
		account := newAccount(t, pool, "tm-team-cascade@example.com")

		if err := join(team, account, "owner"); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team); err != nil {
			t.Fatalf("delete team: %v", err)
		}
		if got := countMembers(t, team); got != 0 {
			t.Errorf("%d memberships survived their team, want 0", got)
		}
	})

	t.Run("deleting the account removes the membership", func(t *testing.T) {
		team := newTeamIn(t, "tm-account-cascade")
		account := newAccount(t, pool, "tm-account-cascade@example.com")

		if err := join(team, account, "member"); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, account); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		if got := countMembers(t, team); got != 0 {
			t.Errorf("%d memberships survived their account, want 0", got)
		}
	})

	// Deleting a workspace reaches team_member through team. No single
	// table's own test covers a break one level up.
	t.Run("deleting the workspace cascades all the way to the team membership", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "tm-full-chain")
		team := newTeam(t, pool, workspace, "Platform")
		account := newAccount(t, pool, "tm-full-chain@example.com")

		if err := join(team, account, "member"); err != nil {
			t.Fatalf("join: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, workspace); err != nil {
			t.Fatalf("delete workspace: %v", err)
		}
		if got := countMembers(t, team); got != 0 {
			t.Errorf("%d memberships survived their workspace, want 0", got)
		}
	})

	t.Run("account_id leads an index and team_id follows", func(t *testing.T) {
		if !memberIndexLeads(t, pool, "team_member", "account_id", "team_id") {
			t.Error("no index on team_member leads with account_id followed by team_id, " +
				"so \"which teams am I in\" scans the table")
		}
	})

	t.Run("team_id leads an index, via the primary key", func(t *testing.T) {
		if !memberIndexLeads(t, pool, "team_member", "team_id", "account_id") {
			t.Error("no index on team_member leads with team_id, so the primary key " +
				"no longer covers the team-side foreign key")
		}
	})
}

// The decision LAM-42 exists to protect, in both directions.
//
// The composite-FK pattern from 0013 and 0018 could force a team member to
// also be a workspace member. It was considered and rejected, because an
// outside collaborator on a single team is a case the product wants. These
// tests are what a future change restoring that constraint has to argue with.
func TestMembershipLevelsAreIndependent(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	t.Run("a team member need not be a workspace member", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "outside-collaborator")
		team := newTeam(t, pool, workspace, "Platform")
		account := newAccount(t, pool, "collaborator@example.com")

		if _, err := pool.Exec(ctx,
			`INSERT INTO team_member (team_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, 'member')`, team, account,
		); err != nil {
			t.Fatalf("outside collaborator on a team: %v", err)
		}

		var inWorkspace int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_member
              WHERE workspace_id = $1::uuid AND account_id = $2::uuid`, workspace, account,
		).Scan(&inWorkspace); err != nil {
			t.Fatalf("check workspace membership: %v", err)
		}
		if inWorkspace != 0 {
			t.Errorf("the collaborator picked up %d workspace memberships, want 0", inWorkspace)
		}
	})

	t.Run("a workspace member need not belong to any team", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "teamless-member")
		newTeam(t, pool, workspace, "Platform")
		account := newAccount(t, pool, "teamless@example.com")

		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_member (workspace_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, 'admin')`, workspace, account,
		); err != nil {
			t.Fatalf("workspace member with no team: %v", err)
		}

		var inTeams int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM team_member WHERE account_id = $1::uuid`, account,
		).Scan(&inTeams); err != nil {
			t.Fatalf("check team membership: %v", err)
		}
		if inTeams != 0 {
			t.Errorf("the workspace member picked up %d team memberships, want 0", inTeams)
		}
	})

	// The roles are independent too: the same vocabulary at both levels, but
	// nothing says they have to agree.
	t.Run("the two levels may hold different roles", func(t *testing.T) {
		workspace := newWorkspace(t, pool, "different-roles")
		team := newTeam(t, pool, workspace, "Platform")
		account := newAccount(t, pool, "different-roles@example.com")

		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_member (workspace_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, 'member')`, workspace, account,
		); err != nil {
			t.Fatalf("workspace membership: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO team_member (team_id, account_id, role)
             VALUES ($1::uuid, $2::uuid, 'owner')`, team, account,
		); err != nil {
			t.Fatalf("team owner who is only a workspace member: %v", err)
		}
	})
}

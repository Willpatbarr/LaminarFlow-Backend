/*
╔═ members.go ══════════════════════════════════════════════════════════════════════════
║  account · who is in the caller's workspace
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Member            struct
║      MemberFields      filter.Schema
║      MemberParams      struct
║      MemberPage        struct
║      Service.Members   method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/members/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package account

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

/*
┏━ Member ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one person in the caller's workspace, as a picker or a byline needs them
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      AccountID      string
┃      DisplayName    string
┃      Email          string
┃      Role           string    owner, admin or member (workspace_member.role)
┃      JoinedAt       time.Time workspace_member.created_at
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Service.Members
*/

type Member struct {
	AccountID   string
	DisplayName string
	Email       string
	Role        string
	JoinedAt    time.Time
}

// MemberFields is the member filter vocabulary. Never password_hash: the column list in
// Members is fixed, and nothing here names it.
var MemberFields = filter.Schema{
	"display_name": {Expr: "a.display_name", Kind: filter.Text},
	"email":        {Expr: "a.email", Kind: filter.Text},
	"role":         {Expr: "wm.role", Kind: filter.Text},
	"created_at":   {Expr: "wm.created_at", Kind: filter.Time},
}

// MemberTiebreak completes every member ordering with the primary key.
const MemberTiebreak = "a.id"

// MemberDefaultSort is display name, ascending: what a picker wants.
var MemberDefaultSort = []filter.Order{{Field: "display_name"}, {Field: "email"}}

/*
┏━ MemberParams ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what one page request asks for
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Filter    filter.Group     empty means everyone
┃      Sort      []filter.Order   empty means MemberDefaultSort
┃      Limit     int              0 means filter.DefaultLimit
┃      Cursor    string           "" means the first page
*/

type MemberParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

// MemberPage is one page of members and how to ask for the next.
type MemberPage struct {
	Members    []Member
	NextCursor string
	HasMore    bool
}

/*
┌─ account ───────────────────────────────────────
│  one page of the people in the caller's workspace
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            MemberParams
├─ out ───────────────────────────────────────────
│      MemberPage
│      error        *filter.Error · filter.ErrCursorMismatch
├─ example ───────────────────────────────────────
│      workspace of 3  →  3 Members, by display name
*/

// Members scopes to the caller's workspace - WorkspaceOf, the answer `me` gives - before
// the filter is consulted, so no body can widen it. The set is workspace_member and not
// team_member on purpose: a review request accepts only workspace members as reviewers
// (internal/comment), and nothing writes team_member yet, so a team roster would be empty
// and would also offer people the reviewer check then refuses.
//
// The column list is fixed. password_hash is not selectable and not filterable.
func (s *Service) Members(ctx context.Context, accountID string, p MemberParams) (MemberPage, error) {
	plan, err := MemberFields.Plan(filter.Request(p), MemberTiebreak, MemberDefaultSort, 2)
	if err != nil {
		return MemberPage{}, err
	}

	args := append([]any{accountID}, plan.Args...)
	args = append(args, plan.Limit+1)

	query := `SELECT a.id::text, a.display_name, a.email, wm.role, wm.created_at, ` +
		strings.Join(plan.Keys, ", ") + `
	            FROM workspace_member wm
	            JOIN account a ON a.id = wm.account_id
	           WHERE wm.workspace_id = ` + WorkspaceOf("$1::uuid") + `
	             AND (` + plan.Where + `) ` +
		plan.Order + ` LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return MemberPage{}, fmt.Errorf("account: members: %w", err)
	}
	defer rows.Close()

	var (
		page MemberPage
		last []*string
	)

	for rows.Next() {
		var (
			m   Member
			key = make([]*string, len(plan.Keys))
		)

		dest := []any{&m.AccountID, &m.DisplayName, &m.Email, &m.Role, &m.JoinedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}

		if err := rows.Scan(dest...); err != nil {
			return MemberPage{}, fmt.Errorf("account: members scan: %w", err)
		}

		if len(page.Members) == plan.Limit {
			page.HasMore = true
			break
		}

		page.Members = append(page.Members, m)
		last = key
	}
	if err := rows.Err(); err != nil {
		return MemberPage{}, fmt.Errorf("account: members rows: %w", err)
	}

	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

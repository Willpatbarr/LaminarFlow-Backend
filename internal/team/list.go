/*
╔═ list.go ═════════════════════════════════════════════════════════════════════════════
║  team · one page of the workspace's teams, statuses included
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ListParams      struct
║      Page            struct
║      Service.List    method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/teams/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package team

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

/*
┏━ ListParams ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what one page request asks for
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Filter    filter.Group     empty means everything
┃      Sort      []filter.Order   empty means DefaultSort
┃      Limit     int              0 means DefaultLimit
┃      Cursor    string           "" means the first page
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api  →  the list request body
*/

type ListParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

/*
┏━ Page ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one page, and how to ask for the next
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Teams         []Team      Statuses filled, AspectTypes not
┃      NextCursor    string      "" on the last page
┃      HasMore       bool
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      List                      one per request
*/

type Page struct {
	Teams      []Team
	NextCursor string
	HasMore    bool
}

/*
┌─ team ──────────────────────────────────────────
│  one page of the caller's workspace's teams
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            ListParams
├─ out ───────────────────────────────────────────
│      Page                      statuses in position order
│      error        *filter.Error · ErrCursorMismatch
├─ example ───────────────────────────────────────
│      2 teams, 3 statuses each  →  2 Teams, 6 Statuses
*/

// List scopes to the caller's workspace - account.WorkspaceOf, the same answer `me`
// gives - before the filter is consulted. Fields has no workspace field, so no body can
// widen it (conventions §5).
//
// Statuses come from a second statement keyed on the page's team ids, not a join: a
// join repeats the team columns per status and makes LIMIT count statuses, so a page of
// 50 would be 17 teams. Two statements is also what keeps the page boundary honest.
// Aspect types are not loaded, as on Get - the client enumerating teams to render a
// ticket list has no use for them, and Get's body has never carried them either.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	// $1 is the caller. Everything the body contributes is numbered after it.
	plan, err := Fields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return Page{}, err
	}

	args := append([]any{accountID}, plan.Args...)
	args = append(args, plan.Limit+1)

	query := `SELECT tm.id::text, tm.workspace_id::text, tm.name, tm.created_at, tm.updated_at, ` +
		strings.Join(plan.Keys, ", ") + `
	            FROM team tm
	           WHERE tm.workspace_id = ` + account.WorkspaceOf("$1::uuid") + `
	             AND (` + plan.Where + `) ` +
		plan.Order + ` LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("team: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)

	for rows.Next() {
		var (
			t   Team
			key = make([]*string, len(plan.Keys))
		)

		dest := []any{&t.ID, &t.WorkspaceID, &t.Name, &t.CreatedAt, &t.UpdatedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}

		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("team: list scan: %w", err)
		}

		if len(page.Teams) == plan.Limit {
			// The sentinel row: its existence is the whole answer.
			page.HasMore = true
			break
		}

		page.Teams = append(page.Teams, t)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("team: list rows: %w", err)
	}
	rows.Close()

	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	if err := s.attachStatuses(ctx, page.Teams); err != nil {
		return Page{}, err
	}

	return page, nil
}

// attachStatuses fills Statuses on every team in one statement. Ordered by position then
// id, the order Statuses gives one team, so a team reads the same through either path.
func (s *Service) attachStatuses(ctx context.Context, teams []Team) error {
	if len(teams) == 0 {
		return nil
	}

	ids := make([]string, len(teams))
	at := make(map[string]int, len(teams))
	for i, t := range teams {
		ids[i] = t.ID
		at[t.ID] = i
	}

	rows, err := s.pool.Query(ctx,
		`SELECT team_id::text, id::text, name, color, position, category FROM status
		  WHERE team_id = ANY($1::uuid[]) ORDER BY team_id, position, id`, ids)
	if err != nil {
		return fmt.Errorf("team: list statuses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			teamID string
			st     Status
		)
		if err := rows.Scan(&teamID, &st.ID, &st.Name, &st.Color, &st.Position, &st.Category); err != nil {
			return fmt.Errorf("team: list statuses scan: %w", err)
		}

		i := at[teamID]
		teams[i].Statuses = append(teams[i].Statuses, st)
	}

	return rows.Err()
}

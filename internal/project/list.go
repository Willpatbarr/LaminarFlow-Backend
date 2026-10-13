/*
╔═ list.go ═════════════════════════════════════════════════════════════════════════════
║  project · one page of the workspace's projects
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ListParams      struct
║      Page            struct
║      Service.List    method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/projects/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package project

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
┃      Projects      []Project   without boards, as Get
┃      NextCursor    string      "" on the last page
┃      HasMore       bool
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      List                      one per request
*/

type Page struct {
	Projects   []Project
	NextCursor string
	HasMore    bool
}

/*
┌─ project ───────────────────────────────────────
│  one page of the caller's workspace's projects
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            ListParams
├─ out ───────────────────────────────────────────
│      Page
│      error        *filter.Error · ErrCursorMismatch
├─ example ───────────────────────────────────────
│      team eq X, limit 50  →  X's projects + cursor
*/

// List scopes through team to the caller's workspace - account.WorkspaceOf, the same
// answer `me` gives - before the filter is consulted. A project has no workspace column;
// it reaches one through its team, which is the join every other project read here
// already makes. Fields has no workspace field, so no body can widen the scope
// (conventions §5).
//
// Boards are not loaded, as on Get: a list is for choosing a project, and a board per
// row is a query per row for something the chooser does not show.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	// $1 is the caller. Everything the body contributes is numbered after it.
	plan, err := Fields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return Page{}, err
	}

	args := append([]any{accountID}, plan.Args...)
	args = append(args, plan.Limit+1)

	query := `SELECT p.id::text, p.team_id::text, p.name, p.created_at, p.updated_at, ` +
		strings.Join(plan.Keys, ", ") + `
	            FROM project p
	            JOIN team tm ON tm.id = p.team_id
	           WHERE tm.workspace_id = ` + account.WorkspaceOf("$1::uuid") + `
	             AND (` + plan.Where + `) ` +
		plan.Order + ` LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("project: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)

	for rows.Next() {
		var (
			pr  Project
			key = make([]*string, len(plan.Keys))
		)

		dest := []any{&pr.ID, &pr.TeamID, &pr.Name, &pr.CreatedAt, &pr.UpdatedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}

		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("project: list scan: %w", err)
		}

		if len(page.Projects) == plan.Limit {
			// The sentinel row: its existence is the whole answer.
			page.HasMore = true
			break
		}

		page.Projects = append(page.Projects, pr)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("project: list rows: %w", err)
	}

	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

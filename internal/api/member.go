/*
╔═ member.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · the people in the caller's workspace
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      memberBody    struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  POST /api/v1/members/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"

	"github.com/danielgtaylor/huma/v2"
)

type memberBody struct {
	AccountID   string    `json:"account_id" format:"uuid" doc:"The id comments carry as author_id and review requests as reviewers[].account_id."`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email"`
	Role        string    `json:"role" enum:"owner,admin,member" doc:"Workspace role."`
	CreatedAt   time.Time `json:"created_at" doc:"When the person joined the workspace."`
}

/*
┌─ api ───────────────────────────────────────────
│  registers the workspace member list
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *account.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/members/list
*/

// The members of the caller's workspace, which is who can be named as a reviewer. It is the
// workspace and not a team: see account.Service.Members. team:read because it is a
// directory read like the team list, and a token scoped for tickets only has no business
// enumerating people.
func registerMembers(api huma.API, svc *account.Service) {
	registerList(api, listSpec[memberBody]{
		OperationID: "list-members",
		Permission:  auth.TeamRead,
		Path:        V1 + "/members/list",
		Summary:     "List the people in the caller's workspace (a read - see the description for why it is a POST)",
		Fields:      account.MemberFields,
		Run: func(ctx context.Context, accountID string, req listRequest) (listPage[memberBody], error) {
			p := account.MemberParams{Sort: req.Sort, Limit: req.Limit, Cursor: req.Cursor}
			if req.Filter != nil {
				p.Filter = *req.Filter
			}

			page, err := svc.Members(ctx, accountID, p)
			if err != nil {
				return listPage[memberBody]{}, err
			}

			items := make([]memberBody, len(page.Members))
			for i, m := range page.Members {
				items[i] = memberBody{
					AccountID: m.AccountID, DisplayName: m.DisplayName, Email: m.Email,
					Role: m.Role, CreatedAt: m.JoinedAt,
				}
			}

			return listPage[memberBody]{Items: items, NextCursor: page.NextCursor, HasMore: page.HasMore}, nil
		},
	})
}

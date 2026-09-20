/*
╔═ account.go ══════════════════════════════════════════════════════════════════════════
║  http handlers · account deletion
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeAccountNotFound    const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  DELETE /api/v1/accounts/me
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"

	"github.com/danielgtaylor/huma/v2"
)

const CodeAccountNotFound = "account_not_found"

// Self-deletion only, and that is a scope decision rather than a missing feature.
//
// Deleting someone else's account needs an authority model - who may, over whom, in
// which workspace - and nothing in this schema defines one. workspace_member.role
// exists but no ticket has said what a role permits. Inventing that here would be
// inventing structure nobody asked for, and the irreversible operation is the worst
// possible place to guess.
type deleteAccountOutput struct {
	// Clearing the cookie is belt and braces: the session rows CASCADE with the
	// account, so the credential is already dead. Leaving the browser holding it
	// would mean every subsequent request 401ing with a cookie the user cannot see
	// to remove.
	SetCookie http.Cookie `header:"Set-Cookie"`

	Body struct {
		PrivateViewsDeleted int `json:"private_views_deleted" doc:"Private saved views removed with the account. Nobody else could see them."`
		SharedViewsKept     int `json:"shared_views_kept" doc:"Shared views left for the team, now owned by nobody."`
	}
}

func accountError(err error) error {
	if errors.Is(err, account.ErrNotFound) {
		return NewError(http.StatusNotFound, CodeAccountNotFound, "no such account")
	}

	return err
}

/*
┌─ api ───────────────────────────────────────────
│  registers account deletion
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *account.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  DELETE /api/v1/accounts/me
*/

func registerAccounts(api huma.API, svc *account.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "delete-account",
		Method:      http.MethodDelete,
		Path:        V1 + "/accounts/me",
		Summary:     "Delete your own account (this one does delete)",
		Description: "Irreversible, unlike archiving a ticket. Private saved views go with the account - " +
			"nobody else could see them, and left behind they would be owned by nobody and visible to " +
			"nobody. Shared views stay with the team, now ownerless. Comments and ticket assignments " +
			"keep their rows and lose their name: work outlives the people who touched it.\n\n" +
			"Only your own account. Deleting someone else's would need an authority model this schema " +
			"does not define.",
		Metadata: map[string]any{RequireAuth: true},
	}, func(ctx context.Context, _ *struct{}) (*deleteAccountOutput, error) {
		me, ok := auth.FromContext(ctx)
		if !ok {
			// Unreachable while the middleware honours RequireAuth. Kept so a
			// metadata typo fails closed rather than deleting nothing under a
			// zero identity - or, worse, an empty uuid that matches a row.
			return nil, huma.Error401Unauthorized("this endpoint requires a credential")
		}

		removal, err := svc.Delete(ctx, me.AccountID)
		if err != nil {
			return nil, accountError(err)
		}

		out := &deleteAccountOutput{SetCookie: *auth.SessionCookie("", time.Time{})}
		out.Body.PrivateViewsDeleted = removal.PrivateViewsDeleted
		out.Body.SharedViewsKept = removal.SharedViewsKept

		return out, nil
	})
}

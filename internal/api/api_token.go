/*
╔═ api_token.go ════════════════════════════════════════════════════════════════════════
║  http handlers · api tokens
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeUnknownScope        const
║      CodeTokenNotFound       const
║      CodeInsufficientScope   const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  registerAuth  →  create · list · revoke
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/danielgtaylor/huma/v2"
)

const (
	// CodeUnknownScope is a 422 at body.scopes[i] for a scope that is not a permission name.
	CodeUnknownScope = "unknown_scope"

	// CodeTokenNotFound is a 404 for a token that is absent, another account's, or already revoked.
	CodeTokenNotFound = "token_not_found"

	// CodeInsufficientScope is a 403 for a token that lacks the permission an operation
	// declares, or that calls an operation only a session may.
	CodeInsufficientScope = "insufficient_scope"
)

type tokenBody struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes" nullable:"false" doc:"Permission names, as resource:action."`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at" doc:"Null never expires."`
	LastUsedAt *time.Time `json:"last_used_at" doc:"Null until first used. Updated at most once a minute, so it can trail the real last use."`
}

type createdTokenBody struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Scopes    []string   `json:"scopes" nullable:"false"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at" doc:"Null never expires."`
	Token     string     `json:"token" doc:"The credential, for the Authorization: Bearer header. Shown here and nowhere else; it cannot be recovered."`
}

type createTokenInput struct {
	Body struct {
		Label     string     `json:"label" required:"true" minLength:"1" maxLength:"100" doc:"Shown in the token list. 1 to 100 characters, and not only whitespace." example:"CI deploy key"`
		Scopes    []string   `json:"scopes" required:"true" nullable:"false" doc:"Permission names, as in the permissions of GET /auth/me. An unknown name is 422 unknown_scope. An empty list makes a token that can do nothing." example:"ticket:read"`
		ExpiresAt *time.Time `json:"expires_at,omitempty" required:"false" nullable:"true" doc:"Must be in the future. Null or omitted never expires."`
	}
}

type createTokenOutput struct {
	Body createdTokenBody
}

type listTokensOutput struct {
	Body struct {
		Tokens []tokenBody `json:"tokens" nullable:"false" doc:"Active tokens only, newest first. Never the token itself."`
	}
}

type revokeTokenInput struct {
	ID string `path:"id" format:"uuid"`
}

func asScopeNames(scopes []auth.Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}

	return out
}

func asTokenBody(t auth.Token) tokenBody {
	return tokenBody{
		ID:         t.ID,
		Label:      t.Label,
		Scopes:     asScopeNames(t.Scopes),
		CreatedAt:  t.CreatedAt.UTC(),
		ExpiresAt:  utcOrNil(t.ExpiresAt),
		LastUsedAt: utcOrNil(t.LastUsedAt),
	}
}

/*
┌─ api ───────────────────────────────────────────
│  registers token create, list and revoke
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *auth.Service       nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /auth/tokens, POST /auth/tokens/list, DELETE /auth/tokens/{id}
*/

func registerAPITokens(api huma.API, svc *auth.Service) {
	sessionOnly := map[string]any{RequireAuth: true, RequireSession: true}

	huma.Register(api, huma.Operation{
		OperationID:   "create-api-token",
		Method:        http.MethodPost,
		Path:          V1 + "/auth/tokens",
		Summary:       "Create an API token",
		DefaultStatus: http.StatusCreated,
		Description: "Session only: a token caller gets 403 `insufficient_scope`, so a leaked token cannot mint more. " +
			"The response carries the credential in `token`; it is returned this once and only a hash is kept.\n\n" +
			"A token may do only what its `scopes` name. An unknown scope is 422 `unknown_scope` and nothing is created.",
		Metadata: sessionOnly,
	}, func(ctx context.Context, in *createTokenInput) (*createTokenOutput, error) {
		me, _ := auth.FromContext(ctx)

		label := strings.TrimSpace(in.Body.Label)
		if label == "" {
			return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"the label was not accepted", "body.label", "must not be blank")
		}

		scopes := make([]auth.Scope, 0, len(in.Body.Scopes))
		seen := map[string]bool{}
		for i, name := range in.Body.Scopes {
			if _, ok := auth.ParsePermission(name); !ok {
				return nil, newFieldError(CodeUnknownScope, "unknown scope "+name,
					fmt.Sprintf("body.scopes[%d]", i), "not a permission name")
			}
			if !seen[name] {
				seen[name] = true
				scopes = append(scopes, auth.Scope(name))
			}
		}

		if in.Body.ExpiresAt != nil && !in.Body.ExpiresAt.After(time.Now()) {
			return nil, newFieldError(defaultCode(http.StatusUnprocessableEntity),
				"the expiry was not accepted", "body.expires_at", "must be in the future")
		}

		tok, secret, err := svc.Create(ctx, me.AccountID, label, scopes, in.Body.ExpiresAt)
		if err != nil {
			return nil, err
		}

		out := &createTokenOutput{}
		out.Body = createdTokenBody{
			ID:        tok.ID,
			Label:     tok.Label,
			Scopes:    asScopeNames(tok.Scopes),
			CreatedAt: tok.CreatedAt.UTC(),
			ExpiresAt: utcOrNil(tok.ExpiresAt),
			Token:     secret,
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-api-tokens",
		Method:      http.MethodPost,
		Path:        V1 + "/auth/tokens/list",
		Summary:     "List your active API tokens",
		Description: "Session only. Revoked and expired tokens are left out. Never returns a token or its hash.",
		Metadata:    sessionOnly,
	}, func(ctx context.Context, _ *struct{}) (*listTokensOutput, error) {
		me, _ := auth.FromContext(ctx)

		tokens, err := svc.List(ctx, me.AccountID)
		if err != nil {
			return nil, err
		}

		out := &listTokensOutput{}
		out.Body.Tokens = make([]tokenBody, len(tokens))
		for i, t := range tokens {
			out.Body.Tokens[i] = asTokenBody(t)
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "revoke-api-token",
		Method:        http.MethodDelete,
		Path:          V1 + "/auth/tokens/{id}",
		Summary:       "Revoke an API token",
		DefaultStatus: http.StatusNoContent,
		Description: "Session only. The token stops authenticating at once. 404 `token_not_found` for a token that is " +
			"absent, another account's, or already revoked.",
		Metadata: sessionOnly,
	}, func(ctx context.Context, in *revokeTokenInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		if err := svc.Revoke(ctx, me.AccountID, in.ID); err != nil {
			if errors.Is(err, auth.ErrTokenNotFound) {
				return nil, NewError(http.StatusNotFound, CodeTokenNotFound, "no such token")
			}

			return nil, err
		}

		return nil, nil
	})
}

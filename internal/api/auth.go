/*
╔═ auth.go ═════════════════════════════════════════════════════════════════════════════
║  http handlers · authentication
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      RequireAuth    operation metadata key
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  login · logout · me, and the middleware
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/danielgtaylor/huma/v2"
)

// RequireAuth marks an operation as needing a caller. The middleware reads it
// off Operation.Metadata and rejects before the handler runs.
//
// Metadata rather than a wrapper around huma.Register: the declaration then
// sits with the operation a reader is already looking at, and a route cannot
// be protected in one file and registered in another.
const RequireAuth = "auth:required"

// A missing credential and a wrong password are both 401, and a caller has to
// tell them apart: one means "log in", the other means "you got it wrong".
// A missing credential takes LAM-53's derived default, "unauthorized".
const CodeInvalidLogin = "invalid_login"

type loginInput struct {
	Body struct {
		Email    string `json:"email" required:"true" doc:"Matched case-insensitively." example:"will@example.com"`
		Password string `json:"password" required:"true" doc:"Never logged, never echoed."`
	}
}

type logoutInput struct {
	Session string `cookie:"__Host-laminar_session"`
}

type meOutput struct {
	Body struct {
		AccountID string `json:"account_id" doc:"The authenticated account."`
		Via       string `json:"via" doc:"Which credential authenticated this request." example:"session"`
	}
}

// logoutOutput carries the cookie that clears the browser's copy. The row is
// already gone by the time this is written; the header is housekeeping.
type logoutOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

type loginOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      struct {
		AccountID string `json:"account_id"`
	}
}

/*
┌─ api ───────────────────────────────────────────
│  registers login, logout and me on the api
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *auth.Service      nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/auth/login, /logout, GET /me
*/

func registerAuth(api huma.API, svc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        V1 + "/auth/login",
		Summary:     "Log in and receive a session cookie",
	}, func(ctx context.Context, in *loginInput) (*loginOutput, error) {
		id, expiresAt, err := svc.LogIn(ctx, in.Body.Email, in.Body.Password)
		if err != nil {
			// One answer for an unknown email and a wrong password alike. The
			// service already equalises the timing; this equalises the response.
			return nil, NewError(http.StatusUnauthorized, CodeInvalidLogin,
				"email or password is incorrect")
		}

		out := &loginOutput{SetCookie: *auth.SessionCookie(id, expiresAt)}

		// The account id is safe to return: the caller just proved it is theirs.
		who, err := svc.ValidateSession(ctx, id)
		if err != nil {
			return nil, err
		}
		out.Body.AccountID = who.AccountID

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "logout",
		Method:      http.MethodPost,
		Path:        V1 + "/auth/logout",
		Summary:     "Revoke the current session",
	}, func(ctx context.Context, in *logoutInput) (*logoutOutput, error) {
		// Deletes the row, which is the whole reason sessions are a table: a
		// cleared cookie only forgets the credential, it does not revoke it.
		if err := svc.LogOut(ctx, in.Session); err != nil {
			return nil, err
		}

		// Deliberately not RequireAuth. Logging out with a dead cookie should
		// clear the browser's copy and report success, not 401 - a caller
		// trying to end a session must never be told to authenticate first.
		return &logoutOutput{SetCookie: *auth.SessionCookie("", time.Time{})}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        V1 + "/auth/me",
		Summary:     "Who the current credential belongs to",
		Metadata:    map[string]any{RequireAuth: true},
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		id, ok := auth.FromContext(ctx)
		if !ok {
			// Unreachable while the middleware honours RequireAuth. Kept so a
			// metadata typo fails closed rather than serving a zero Identity.
			return nil, huma.Error401Unauthorized("this endpoint requires a credential")
		}

		out := &meOutput{}
		out.Body.AccountID = id.AccountID
		out.Body.Via = "token"
		if id.SessionID != "" {
			out.Body.Via = "session"
		}

		return out, nil
	})
}

/*
┌─ api ───────────────────────────────────────────
│  resolves either credential before a handler runs
├─ in ────────────────────────────────────────────
│      api    huma.API           for WriteErr's envelope
│      svc    *auth.Service
├─ out ───────────────────────────────────────────
│      huma middleware
├─ example ───────────────────────────────────────
│      RequireAuth op, no cookie  →  401 unauthenticated
*/

func authenticate(api huma.API, svc *auth.Service) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		required := false
		if op := ctx.Operation(); op != nil {
			required, _ = op.Metadata[RequireAuth].(bool)
		}

		var cookie string
		if c, err := huma.ReadCookie(ctx, auth.SessionCookieName); err == nil && c != nil {
			cookie = c.Value
		}

		id, err := svc.Authenticate(ctx.Context(), ctx.Header("Authorization"), cookie)
		switch {
		case err == nil:
			// Resolved. Attached whether or not the operation demanded it, so a
			// public endpoint can still personalise without its own lookup.
			ctx = huma.WithContext(ctx, auth.NewContext(ctx.Context(), id))
		case required:
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized,
				"this endpoint requires a credential")
			return
		}

		next(ctx)
	}
}

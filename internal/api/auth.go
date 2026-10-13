/*
╔═ auth.go ═════════════════════════════════════════════════════════════════════════════
║  http handlers · authentication
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      RequireAuth             operation metadata key
║      CodeInvalidLogin        const
║      CodeEmailTaken          const
║      CodePasswordTooShort    const
║      CodePasswordTooLong     const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  register · login · logout · me, and the middleware
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

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
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

// The three ways a registration is refused for reasons the caller can fix. Each has a
// code because each wants a different message beside a different field: "that address is
// taken" belongs under the email box, the other two under the password box.
const (
	CodeEmailTaken       = "email_taken"
	CodePasswordTooShort = "password_too_short"
	CodePasswordTooLong  = "password_too_long"
)

type registerInput struct {
	Body struct {
		Email       string `json:"email" required:"true" format:"email" maxLength:"254" doc:"Matched case-insensitively. Must not already be registered." example:"will@example.com"`
		Password    string `json:"password" required:"true" doc:"At least 10 characters and at most 72 bytes. Never logged, never echoed."`
		DisplayName string `json:"display_name" required:"true" minLength:"1" maxLength:"100" doc:"What the app shows for this person. Not unique." example:"Will Barr"`
	}
}

// registerOutput sets the cookie exactly as login does, so sign-up lands logged in.
type registerOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      struct {
		AccountID        string `json:"account_id"`
		WorkspaceID      string `json:"workspace_id" doc:"The workspace the account now belongs to."`
		WorkspaceCreated bool   `json:"workspace_created" doc:"True when this registration created the workspace - a fresh install with none."`
	}
}

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
		AccountID   string   `json:"account_id" doc:"The authenticated account."`
		Via         string   `json:"via" doc:"Which credential authenticated this request." example:"session"`
		Email       string   `json:"email"`
		DisplayName string   `json:"display_name" doc:"What the app shows for this person."`
		WorkspaceID *string  `json:"workspace_id" doc:"The workspace this account belongs to. Null only for an account with no membership."`
		Permissions []string `json:"permissions" nullable:"false" doc:"Everything this credential may do, as resource:action. Gate UI on membership in this list, not on its length: there is no authority model yet, so today every credential holds the full set, and roles will narrow it without changing its shape." example:"ticket:write"`
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
│  registers register, login, logout and me
├─ in ────────────────────────────────────────────
│      api         huma.API
│      svc         *auth.Service       nil only for cmd/openapi
│      accounts    *account.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/auth/register, /login, /logout, GET /me
*/

func registerAuth(api huma.API, svc *auth.Service, accounts *account.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "register",
		Method:        http.MethodPost,
		Path:          V1 + "/auth/register",
		Summary:       "Create an account and log in as it",
		DefaultStatus: http.StatusCreated,
		Description: "Public: this is how a person gets a session. Creates the account and, in the same " +
			"transaction, its membership of the workspace - creating the workspace first if the install " +
			"has none. On success it sets the session cookie exactly as login does.\n\n" +
			"Single-workspace model: when a workspace already exists the new account joins it as a member. " +
			"There is no invitation or approval step yet (ADR 0004), so until there is, anyone who can " +
			"reach this endpoint can join - do not expose it beyond a trusted network.\n\n" +
			"A duplicate email answers 409 `email_taken`. That reveals the address is registered, which " +
			"a registration endpoint cannot avoid; it costs the same time as a success.",
	}, func(ctx context.Context, in *registerInput) (*registerOutput, error) {
		name := strings.TrimSpace(in.Body.DisplayName)
		if name == "" {
			// minLength passes " ", so the schema alone would store a blank name.
			return nil, huma.Error422UnprocessableEntity("display name must not be blank",
				&huma.ErrorDetail{Message: "must not be blank", Location: "body.display_name"})
		}

		switch err := auth.ValidatePassword(in.Body.Password); {
		case errors.Is(err, auth.ErrPasswordTooShort):
			return nil, NewError(http.StatusUnprocessableEntity, CodePasswordTooShort,
				fmt.Sprintf("password must be at least %d characters", auth.MinPasswordLength))
		case errors.Is(err, auth.ErrPasswordTooLong):
			return nil, NewError(http.StatusUnprocessableEntity, CodePasswordTooLong,
				fmt.Sprintf("password must be at most %d bytes", auth.MaxPasswordBytes))
		}

		// Hashed before the email is looked at, so a taken address pays the same
		// bcrypt cost a free one does and the response time says nothing the 409
		// does not. The hash is auth's: this package has one implementation of it.
		hash, err := auth.HashPassword(in.Body.Password)
		if err != nil {
			return nil, err
		}

		reg, err := accounts.Register(ctx, account.NewAccount{
			Email:        in.Body.Email,
			PasswordHash: hash,
			DisplayName:  name,
		})
		if errors.Is(err, account.ErrEmailTaken) {
			return nil, NewError(http.StatusConflict, CodeEmailTaken,
				"an account with this email already exists")
		}
		if err != nil {
			return nil, err
		}

		// A separate step from the registration, deliberately. If it fails the
		// account exists and the caller can log in; making the two atomic would mean
		// auth writing inside account's transaction for a failure that costs one
		// extra request to recover from.
		id, expiresAt, err := svc.OpenSession(ctx, reg.AccountID)
		if err != nil {
			return nil, err
		}

		out := &registerOutput{SetCookie: *auth.SessionCookie(id, expiresAt)}
		out.Body.AccountID = reg.AccountID
		out.Body.WorkspaceID = reg.WorkspaceID
		out.Body.WorkspaceCreated = reg.WorkspaceCreated

		return out, nil
	})

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

		profile, err := accounts.Profile(ctx, id.AccountID)
		if errors.Is(err, account.ErrNotFound) {
			// A live credential for a deleted account. The credentials CASCADE with
			// the account, so only a request in flight during the delete gets here.
			return nil, huma.Error401Unauthorized("this endpoint requires a credential")
		}
		if err != nil {
			return nil, err
		}

		out := &meOutput{}
		out.Body.AccountID = id.AccountID
		out.Body.Via = "token"
		if id.SessionID != "" {
			out.Body.Via = "session"
		}
		out.Body.Email = profile.Email
		out.Body.DisplayName = profile.DisplayName
		if profile.WorkspaceID != "" {
			out.Body.WorkspaceID = &profile.WorkspaceID
		}
		out.Body.Permissions = auth.Permissions(id)

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

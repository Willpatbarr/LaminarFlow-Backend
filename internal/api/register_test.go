package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A mux and the pool behind it, for tests that have to look at the tables or mint a
// token. When emptyInstall is set the workspace migration 0003 seeds is deleted first,
// which is the "no workspace exists" case the ticket names; a migrated install always
// has that row, so the other case is the one a real server meets.
func registrationMux(t *testing.T, emptyInstall bool) (*http.ServeMux, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(cleanup)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// dbtest.Create makes an empty database; the migrations are the schema. The
	// older tests in this package never needed one.
	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Up(ctx, pool, loaded); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	if emptyInstall {
		if _, err := pool.Exec(ctx, `DELETE FROM workspace`); err != nil {
			t.Fatalf("empty the install: %v", err)
		}
	}

	return NewMux(pool, bundle, testHub(t)), pool
}

func call(t *testing.T, mux *http.ServeMux, method, path string, body any, cookie string, header ...string) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	}
	if len(header) == 2 {
		req.Header.Set(header[0], header[1])
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

type registered struct {
	AccountID        string `json:"account_id"`
	WorkspaceID      string `json:"workspace_id"`
	WorkspaceCreated bool   `json:"workspace_created"`
}

type who struct {
	AccountID   string   `json:"account_id"`
	Via         string   `json:"via"`
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name"`
	WorkspaceID *string  `json:"workspace_id"`
	Permissions []string `json:"permissions"`
}

func register(t *testing.T, mux *http.ServeMux, email string) (registered, string) {
	t.Helper()

	rec := call(t, mux, http.MethodPost, V1+"/auth/register", map[string]string{
		"email": email, "password": "correct horse battery", "display_name": "Will Barr",
	}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", email, rec.Code, rec.Body)
	}

	var out registered
	decode(t, rec, &out)

	return out, sessionCookieOf(t, rec)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()

	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
}

func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie in %v", auth.SessionCookieName, rec.Header()["Set-Cookie"])

	return ""
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) Error {
	t.Helper()

	var e Error
	decode(t, rec, &e)

	return e
}

// The first sign-up on an install with nothing in it: the account, the workspace and the
// membership all exist afterwards, and the caller is already logged in.
func TestRegisterOnAnEmptyInstallCreatesWorkspaceAndMembershipAndLogsIn(t *testing.T) {
	mux, pool := registrationMux(t, true)

	out, cookie := register(t, mux, "Will@Example.com")

	if !out.WorkspaceCreated {
		t.Error("workspace_created = false on an install with no workspace")
	}
	if n := count(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want 1", n)
	}
	if n := count(t, pool,
		`SELECT count(*) FROM workspace_member WHERE workspace_id = $1::uuid AND account_id = $2::uuid`,
		out.WorkspaceID, out.AccountID); n != 1 {
		t.Errorf("%d membership rows, want 1", n)
	}

	// And it is a working session, not merely a cookie.
	me := call(t, mux, http.MethodGet, V1+"/auth/me", nil, cookie)
	if me.Code != http.StatusOK {
		t.Fatalf("me with the register cookie: status %d, body %s", me.Code, me.Body)
	}

	var got who
	decode(t, me, &got)
	if got.AccountID != out.AccountID || got.Via != "session" {
		t.Errorf("me = %+v, want account %s via session", got, out.AccountID)
	}
}

// "Exactly the way login does": same cookie, same attributes. Compared against a real
// login rather than against constants, so the two cannot drift apart unnoticed.
func TestRegisterSetsTheCookieLoginSets(t *testing.T) {
	mux, _ := registrationMux(t, false)

	rec := call(t, mux, http.MethodPost, V1+"/auth/register", map[string]string{
		"email": "will@example.com", "password": "correct horse battery", "display_name": "Will",
	}, "")
	login := call(t, mux, http.MethodPost, V1+"/auth/login", map[string]string{
		"email": "will@example.com", "password": "correct horse battery",
	}, "")

	find := func(rec *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.SessionCookieName {
				return c
			}
		}
		t.Fatalf("no session cookie in %v", rec.Header()["Set-Cookie"])

		return nil
	}

	r, l := find(rec), find(login)
	if r.Path != l.Path || r.HttpOnly != l.HttpOnly || r.Secure != l.Secure || r.SameSite != l.SameSite {
		t.Errorf("register cookie %+v differs from login cookie %+v", r, l)
	}
	if !r.HttpOnly || !r.Secure || r.MaxAge <= 0 {
		t.Errorf("register cookie is not a persistent HttpOnly Secure cookie: %+v", r)
	}
	if r.Value == l.Value {
		t.Error("register and login returned the same session id")
	}
}

// The case a migrated install is actually in: 0003's workspace is there, empty.
func TestRegisterOnAMigratedInstallUsesTheSeededWorkspace(t *testing.T) {
	mux, pool := registrationMux(t, false)

	out, _ := register(t, mux, "first@example.com")

	if out.WorkspaceCreated {
		t.Error("created a second workspace beside the seeded one")
	}
	if n := count(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want the seeded 1", n)
	}
}

func TestASecondRegistrationJoinsTheExistingWorkspace(t *testing.T) {
	mux, pool := registrationMux(t, true)

	first, _ := register(t, mux, "first@example.com")
	second, _ := register(t, mux, "second@example.com")

	if second.WorkspaceID != first.WorkspaceID {
		t.Errorf("second joined %s, want %s", second.WorkspaceID, first.WorkspaceID)
	}
	if second.WorkspaceCreated {
		t.Error("the second registration created a workspace")
	}
	if n := count(t, pool, `SELECT count(*) FROM workspace`); n != 1 {
		t.Errorf("%d workspaces, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM workspace_member WHERE workspace_id = $1::uuid`,
		first.WorkspaceID); n != 2 {
		t.Errorf("%d members, want 2", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM workspace_member WHERE role = 'member'`); n != 1 {
		t.Errorf("%d plain members, want 1", n)
	}
}

func TestADuplicateEmailIs409WithItsCode(t *testing.T) {
	mux, _ := registrationMux(t, false)
	register(t, mux, "will@example.com")

	// Another case, because login folds case and so must this.
	rec := call(t, mux, http.MethodPost, V1+"/auth/register", map[string]string{
		"email": "WILL@example.com", "password": "correct horse battery", "display_name": "Impostor",
	}, "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	if e := errorOf(t, rec); e.Code != CodeEmailTaken {
		t.Errorf("code = %q, want %q", e.Code, CodeEmailTaken)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a refused registration set a cookie")
	}
}

func TestAPasswordShorterThanTheMinimumIs422AndCreatesNothing(t *testing.T) {
	mux, pool := registrationMux(t, true)

	body := map[string]string{"email": "will@example.com", "password": "short", "display_name": "Will"}

	rec := call(t, mux, http.MethodPost, V1+"/auth/register", body, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if e := errorOf(t, rec); e.Code != CodePasswordTooShort {
		t.Errorf("code = %q, want %q", e.Code, CodePasswordTooShort)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a refused registration set a cookie")
	}

	// The refusal came before the transaction: no workspace was made for it either,
	// and the same address is still free.
	if n := count(t, pool, `SELECT count(*) FROM workspace`); n != 0 {
		t.Errorf("%d workspaces after a refused registration, want 0", n)
	}
	body["password"] = "correct horse battery"
	if rec := call(t, mux, http.MethodPost, V1+"/auth/register", body, ""); rec.Code != http.StatusCreated {
		t.Errorf("retry with a long enough password: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestAPasswordBcryptWouldTruncateIsRefusedWithItsOwnCode(t *testing.T) {
	mux, _ := registrationMux(t, false)

	rec := call(t, mux, http.MethodPost, V1+"/auth/register", map[string]string{
		"email": "will@example.com", "password": strings.Repeat("a", 73), "display_name": "Will",
	}, "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
	if e := errorOf(t, rec); e.Code != CodePasswordTooLong {
		t.Errorf("code = %q, want %q", e.Code, CodePasswordTooLong)
	}
}

func TestMalformedRegistrationsAre422(t *testing.T) {
	mux, _ := registrationMux(t, false)

	for name, body := range map[string]map[string]string{
		"not an email":       {"email": "will", "password": "correct horse battery", "display_name": "Will"},
		"empty display name": {"email": "a@example.com", "password": "correct horse battery", "display_name": ""},
		"blank display name": {"email": "b@example.com", "password": "correct horse battery", "display_name": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			rec := call(t, mux, http.MethodPost, V1+"/auth/register", body, "")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422; body %s", rec.Code, rec.Body)
			}
		})
	}
}

// Registering and then logging in is the round trip a second device makes: it proves the
// stored hash is the one LogIn verifies.
func TestARegisteredAccountCanLogInAgain(t *testing.T) {
	mux, _ := registrationMux(t, false)
	register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodPost, V1+"/auth/login", map[string]string{
		"email": "Will@Example.com", "password": "correct horse battery",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestMeReportsWhoAndWhatForASession(t *testing.T) {
	mux, _ := registrationMux(t, false)
	out, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}

	var got who
	decode(t, rec, &got)

	if got.AccountID != out.AccountID || got.Via != "session" {
		t.Errorf("account %q via %q, want %q via session", got.AccountID, got.Via, out.AccountID)
	}
	if got.Email != "will@example.com" || got.DisplayName != "Will Barr" {
		t.Errorf("email %q, display name %q", got.Email, got.DisplayName)
	}
	if got.WorkspaceID == nil || *got.WorkspaceID != out.WorkspaceID {
		t.Errorf("workspace_id = %v, want %s", got.WorkspaceID, out.WorkspaceID)
	}
	if !slices.Equal(got.Permissions, auth.Permissions(auth.Identity{})) || !slices.Contains(got.Permissions, "ticket:write") {
		t.Errorf("permissions = %v", got.Permissions)
	}

	// A list on the wire even if it were ever empty: a client that does
	// permissions.includes() must never meet null.
	var raw map[string]json.RawMessage
	decode(t, rec, &raw)
	if !bytes.HasPrefix(raw["permissions"], []byte("[")) {
		t.Errorf("permissions on the wire = %s, want a JSON array", raw["permissions"])
	}
}

func TestMeReportsTheSameShapeForAnAPIToken(t *testing.T) {
	mux, pool := registrationMux(t, false)
	out, _ := register(t, mux, "will@example.com")

	token, err := auth.NewService(pool).Issue(context.Background(), out.AccountID, "script", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, "", "Authorization", "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}

	var got who
	decode(t, rec, &got)

	if got.Via != "token" || got.AccountID != out.AccountID {
		t.Errorf("via %q account %q, want token / %s", got.Via, got.AccountID, out.AccountID)
	}
	if got.Email != "will@example.com" || got.DisplayName != "Will Barr" {
		t.Errorf("email %q, display name %q", got.Email, got.DisplayName)
	}
	if got.WorkspaceID == nil || *got.WorkspaceID != out.WorkspaceID {
		t.Errorf("workspace_id = %v, want %s", got.WorkspaceID, out.WorkspaceID)
	}
	if !slices.Equal(got.Permissions, auth.Permissions(auth.Identity{})) {
		t.Errorf("permissions = %v, want the full set (no authority model yet)", got.Permissions)
	}
}

func TestMeStillRequiresACredential(t *testing.T) {
	mux, _ := registrationMux(t, false)

	if rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

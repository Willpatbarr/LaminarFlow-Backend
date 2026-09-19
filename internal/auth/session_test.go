package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// withPassword gives the test account a real bcrypt hash so LogIn can succeed.
func withPassword(t *testing.T, svc *Service, accountID, password string) {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE account SET password_hash = $1 WHERE id = $2`, string(hash), accountID); err != nil {
		t.Fatalf("set password: %v", err)
	}
}

// emailOf reads back the address the throwaway account was created with.
func emailOf(t *testing.T, svc *Service, accountID string) string {
	t.Helper()

	var email string
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT email FROM account WHERE id = $1`, accountID).Scan(&email); err != nil {
		t.Fatalf("read email: %v", err)
	}

	return email
}

// The round trip a browser makes.
func TestLogInOpensAValidSession(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "correct horse battery staple")

	id, expiresAt, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "correct horse battery staple")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !expiresAt.After(time.Now().Add(29 * 24 * time.Hour)) {
		t.Errorf("expiresAt = %v, want roughly 30 days out", expiresAt)
	}

	who, err := svc.ValidateSession(ctx, id)
	if err != nil {
		t.Fatalf("validate session: %v", err)
	}
	if who.AccountID != accountID {
		t.Errorf("account = %q, want %q", who.AccountID, accountID)
	}
	if who.SessionID == "" {
		t.Error("session id is empty, so logout cannot name the row")
	}
	if len(who.Scopes) != 0 {
		t.Errorf("scopes = %v, want none - a person is not scope-limited", who.Scopes)
	}
}

// 0007 indexes lower(email) precisely so a login can match case-insensitively.
func TestLogInMatchesEmailCaseInsensitively(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	if _, _, err := svc.LogIn(ctx, strings.ToUpper(emailOf(t, svc, accountID)), "hunter2"); err != nil {
		t.Errorf("login with an upper-cased address: %v, want success", err)
	}
}

// Unknown email and wrong password must be one answer, or the response
// enumerates who has an account.
func TestLogInFailuresAreIndistinguishable(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	for _, tc := range []struct{ name, email, password string }{
		{"unknown email", "nobody-here@example.com", "hunter2"},
		{"wrong password", emailOf(t, svc, accountID), "not the password"},
		{"empty password", emailOf(t, svc, accountID), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.LogIn(ctx, tc.email, tc.password); !errors.Is(err, ErrInvalidLogin) {
				t.Errorf("err = %v, want ErrInvalidLogin", err)
			}
		})
	}
}

// The reason sessions are a table rather than a signed cookie. A cleared
// cookie only forgets the credential; this proves the credential is dead.
func TestLogOutRevokesTheSession(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, id); err != nil {
		t.Fatalf("validate before logout: %v", err)
	}

	if err := svc.LogOut(ctx, id); err != nil {
		t.Fatalf("logout: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, id); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want the session dead after logout", err)
	}
}

// Logging out twice, or with a cookie that was already revoked, is not an
// error - a caller trying to end a session must never be told to try again.
func TestLogOutIsIdempotent(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	for _, call := range []string{"first", "second", "empty"} {
		value := id
		if call == "empty" {
			value = ""
		}
		if err := svc.LogOut(ctx, value); err != nil {
			t.Errorf("%s logout: %v, want nil", call, err)
		}
	}
}

// The capability a signed cookie cannot offer at all.
func TestLogOutEverywhereRevokesEverySession(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")
	email := emailOf(t, svc, accountID)

	var ids []string
	for i := 0; i < 3; i++ {
		id, _, err := svc.LogIn(ctx, email, "hunter2")
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	n, err := svc.LogOutEverywhere(ctx, accountID)
	if err != nil {
		t.Fatalf("log out everywhere: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked %d, want 3", n)
	}

	for i, id := range ids {
		if _, err := svc.ValidateSession(ctx, id); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("session %d still valid: %v", i, err)
		}
	}
}

// An expired row is refused even though it is still present.
func TestAnExpiredSessionIsRefused(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := svc.pool.Exec(ctx,
		`UPDATE session SET expires_at = now() - interval '1 second' WHERE token_hash = $1`,
		hashSessionID(id)); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, id); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want an expired session refused", err)
	}
}

// 0028 promises the stored value is a hash, so a database dump yields no
// usable cookies.
func TestTheSessionRowDoesNotContainTheCookieValue(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var stored string
	if err := svc.pool.QueryRow(ctx,
		`SELECT token_hash FROM session WHERE account_id = $1`, accountID).Scan(&stored); err != nil {
		t.Fatalf("read row: %v", err)
	}

	if stored == id || strings.Contains(stored, id) {
		t.Error("token_hash holds the cookie value, so a dump hands over live sessions")
	}
	if len(stored) != 64 {
		t.Errorf("token_hash = %q (%d chars), want 64 hex chars of SHA-256", stored, len(stored))
	}
}

// Sliding expiry must not write on every request - that would put an UPDATE in
// front of every page load. A session just opened is already at full life, so
// validating it changes nothing.
func TestValidatingAFreshSessionDoesNotWrite(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var before time.Time
	if err := svc.pool.QueryRow(ctx,
		`SELECT updated_at FROM session WHERE token_hash = $1`, hashSessionID(id)).Scan(&before); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := svc.ValidateSession(ctx, id); err != nil {
			t.Fatalf("validate %d: %v", i, err)
		}
	}

	var after time.Time
	if err := svc.pool.QueryRow(ctx,
		`SELECT updated_at FROM session WHERE token_hash = $1`, hashSessionID(id)).Scan(&after); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	if !after.Equal(before) {
		t.Errorf("updated_at moved from %v to %v - sliding wrote on a fresh session", before, after)
	}
}

// ...but a session past the threshold does get extended, or "sliding" is a
// word with no behaviour behind it.
func TestAnAgingSessionIsExtended(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// Two days of life spent, which is past the one-day threshold.
	if _, err := svc.pool.Exec(ctx,
		`UPDATE session SET expires_at = now() + interval '28 days' WHERE token_hash = $1`,
		hashSessionID(id)); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, id); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var expiresAt time.Time
	if err := svc.pool.QueryRow(ctx,
		`SELECT expires_at FROM session WHERE token_hash = $1`, hashSessionID(id)).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}

	if !expiresAt.After(time.Now().Add(29 * 24 * time.Hour)) {
		t.Errorf("expiresAt = %v, want it slid back out to ~30 days", expiresAt)
	}
}

// 0028's CASCADE, asserted through this package rather than left to the schema
// tests: a session outliving its account is an open door.
func TestDeletingTheAccountRevokesItsSessions(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	id, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := svc.pool.Exec(ctx, `DELETE FROM account WHERE id = $1`, accountID); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, id); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want the session gone with the account", err)
	}
}

// The convergence LAM-55 required: both credentials resolve to one Identity
// before any handler sees either.
func TestAuthenticateAcceptsEitherCredential(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()
	withPassword(t, svc, accountID, "hunter2")

	session, _, err := svc.LogIn(ctx, emailOf(t, svc, accountID), "hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	token, err := svc.Issue(ctx, accountID, "script", []Scope{"tickets:read"}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	t.Run("cookie", func(t *testing.T) {
		id, err := svc.Authenticate(ctx, "", session)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if id.AccountID != accountID || id.SessionID == "" || id.TokenID != "" {
			t.Errorf("identity = %+v, want a session identity", id)
		}
	})

	t.Run("bearer", func(t *testing.T) {
		id, err := svc.Authenticate(ctx, "Bearer "+token, "")
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if id.AccountID != accountID || id.TokenID == "" || id.SessionID != "" {
			t.Errorf("identity = %+v, want a token identity", id)
		}
	})

	t.Run("header wins over a stale cookie", func(t *testing.T) {
		id, err := svc.Authenticate(ctx, "Bearer "+token, "lam_dead_cookie")
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if id.TokenID == "" {
			t.Error("a stale cookie beat an explicit Authorization header")
		}
	})

	t.Run("neither", func(t *testing.T) {
		if _, err := svc.Authenticate(ctx, "", ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("err = %v, want ErrInvalidToken", err)
		}
	})
}

// The cookie attributes are the security boundary, so they are pinned rather
// than trusted to stay right through a refactor.
func TestSessionCookieAttributes(t *testing.T) {
	c := SessionCookie("abc", time.Now().Add(SessionMaxAge))

	if c.Name != SessionCookieName || !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("name = %q, want the __Host- prefix", c.Name)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly is off, so XSS can read the session")
	}
	if !c.Secure {
		t.Error("Secure is off, which the __Host- prefix also forbids")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict - the frontend is same-origin", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want / as the __Host- prefix requires", c.Path)
	}
	if c.Domain != "" {
		t.Errorf("Domain = %q, want empty as the __Host- prefix requires", c.Domain)
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want a persistent cookie so closing the browser keeps you in", c.MaxAge)
	}
}

// Clearing has to be a real deletion instruction, not an empty value the
// browser keeps.
func TestSessionCookieClears(t *testing.T) {
	c := SessionCookie("", time.Time{})

	if c.Value != "" {
		t.Errorf("value = %q, want empty", c.Value)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1 so the browser deletes it", c.MaxAge)
	}
}

// The init-order trap. dummyHash is assigned in init(), so any package-level
// var aliasing it captures nil - and bcrypt against nil returns instantly,
// which is the timing oracle the dummy comparison exists to close. Asserted on
// the value rather than on timing, which would be flaky.
func TestDummyHashIsUsableAtPackageInit(t *testing.T) {
	if len(dummyHash) == 0 {
		t.Fatal("dummyHash is empty, so every unknown-account comparison is a no-op")
	}
	if !strings.HasPrefix(string(dummyHash), "$2") {
		t.Errorf("dummyHash = %q, want a bcrypt hash", dummyHash)
	}
	if bcrypt.CompareHashAndPassword(dummyHash, []byte("anything")) == nil {
		t.Error("dummyHash matched a guess, which should be impossible")
	}
}

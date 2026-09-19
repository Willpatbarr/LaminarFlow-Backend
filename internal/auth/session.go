/*
╔═ session.go ══════════════════════════════════════════════════════════════════════════
║  auth · browser session credential
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      SessionCookieName    const
║      ErrInvalidLogin      error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  login · logout · me · Authenticate
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// SessionCookieName is the cookie the browser carries. The __Host- prefix is a
// browser-enforced promise: Secure, path /, and no Domain, so no subdomain can
// set or overwrite it.
const SessionCookieName = "__Host-laminar_session"

const (
	sessionIDBytes     = 32 // 256 bits, the whole secret
	SessionMaxAge      = 30 * 24 * time.Hour
	sessionExtendAfter = 24 * time.Hour // slide at most once a day, not per request
)

// One error for both halves of a bad login, so nobody can enumerate accounts
// by watching which addresses fail differently.
var ErrInvalidLogin = errors.New("auth: invalid email or password")

/*
┌─ auth ──────────────────────────────────────────
│  verifies a password and opens a session
├─ in ────────────────────────────────────────────
│      ctx         context.Context
│      email       string      matched case-insensitively
│      password    string
├─ out ───────────────────────────────────────────
│      string                  the cookie value, shown once
│      time.Time               expiry, for the cookie's Max-Age
│      error                   always ErrInvalidLogin
├─ example ───────────────────────────────────────
│      "Will@x.com", "hunter2"  →  "k3f2…", +30d
*/

func (s *Service) LogIn(ctx context.Context, email, password string) (string, time.Time, error) {
	var (
		accountID string
		hash      string
	)

	// lower(email) is the indexed expression 0007 built for exactly this read.
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash FROM account WHERE lower(email) = lower($1)`, email,
	).Scan(&accountID, &hash)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Pay the comparison anyway: a missing account must not answer faster
		// than a wrong password, or the timing enumerates who has an account.
		// dummyHash, read here rather than aliased into a package var: Go
		// initialises package variables before init() runs, so an alias would
		// capture nil, compare against nothing, return instantly, and hand back
		// exactly the timing difference this line exists to remove.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", time.Time{}, ErrInvalidLogin
	case err != nil:
		return "", time.Time{}, fmt.Errorf("auth: look up account: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return "", time.Time{}, ErrInvalidLogin
	}

	return s.openSession(ctx, accountID)
}

/*
┌─ auth ──────────────────────────────────────────
│  mints a session row and returns its cookie value
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      string                  raw id, never stored
│      time.Time               expires_at as written
│      error
*/

func (s *Service) openSession(ctx context.Context, accountID string) (string, time.Time, error) {
	id, err := randomString(sessionIDBytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: generate session id: %w", err)
	}

	expiresAt := time.Now().Add(SessionMaxAge)

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO session (account_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		accountID, hashSessionID(id), expiresAt,
	); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: insert session: %w", err)
	}

	return id, expiresAt, nil
}

/*
┌─ auth ──────────────────────────────────────────
│  resolves a cookie value, sliding the deadline
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      presented    string      the raw cookie value
├─ out ───────────────────────────────────────────
│      Identity                 Scopes always nil
│      error                    always ErrInvalidToken
├─ example ───────────────────────────────────────
│      live id  →  Identity{AccountID: "8b1c…"}
*/

func (s *Service) ValidateSession(ctx context.Context, presented string) (Identity, error) {
	if presented == "" {
		return Identity{}, ErrInvalidToken
	}

	var (
		id        string
		accountID string
		expiresAt time.Time
	)

	err := s.pool.QueryRow(ctx,
		`SELECT id, account_id, expires_at FROM session WHERE token_hash = $1`,
		hashSessionID(presented),
	).Scan(&id, &accountID, &expiresAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No dummy comparison needed here, unlike the token path: the lookup is
		// a hash compare in the index, so a hit and a miss already cost the same.
		return Identity{}, ErrInvalidToken
	case err != nil:
		return Identity{}, fmt.Errorf("auth: look up session: %w", err)
	}

	if !expiresAt.After(time.Now()) {
		return Identity{}, ErrInvalidToken
	}

	// Sliding expiry, written only when the deadline has moved more than a day
	// from where it would be now. Extending on every request would put an
	// UPDATE in front of every page load to buy nothing.
	if time.Until(expiresAt) < SessionMaxAge-sessionExtendAfter {
		if _, err := s.pool.Exec(ctx,
			`UPDATE session SET expires_at = $1, updated_at = now() WHERE id = $2`,
			time.Now().Add(SessionMaxAge), id,
		); err != nil {
			return Identity{}, fmt.Errorf("auth: extend session: %w", err)
		}
	}

	// Scopes stay nil. A person in a browser is not scope-limited; scopes exist
	// so a script can be handed less than its owner has.
	return Identity{AccountID: accountID, SessionID: id}, nil
}

/*
┌─ auth ──────────────────────────────────────────
│  revokes one session, the thing a cookie cannot do
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      presented    string      the raw cookie value
├─ out ───────────────────────────────────────────
│      error                    nil when already gone
*/

func (s *Service) LogOut(ctx context.Context, presented string) error {
	if presented == "" {
		return nil
	}

	if _, err := s.pool.Exec(ctx,
		`DELETE FROM session WHERE token_hash = $1`, hashSessionID(presented),
	); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}

	return nil
}

/*
┌─ auth ──────────────────────────────────────────
│  revokes every session an account holds
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      int                      rows revoked
│      error
├─ example ───────────────────────────────────────
│      3 live browsers  →  3
*/

func (s *Service) LogOutEverywhere(ctx context.Context, accountID string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM session WHERE account_id = $1`, accountID)
	if err != nil {
		return 0, fmt.Errorf("auth: delete sessions: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

/*
┌─ auth ──────────────────────────────────────────
│  the cookie carrying a session, with its attributes
├─ in ────────────────────────────────────────────
│      value       string       empty clears the cookie
│      expiresAt   time.Time    ignored when value is empty
├─ out ───────────────────────────────────────────
│      *http.Cookie
├─ example ───────────────────────────────────────
│      "" , zero  →  MaxAge -1, browser deletes it
*/

func SessionCookie(value string, expiresAt time.Time) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true, // script cannot read it, so XSS cannot steal it
		Secure:   true, // required by the __Host- prefix, and correct regardless
		SameSite: http.SameSiteStrictMode,
	}

	// Strict is viable because internal/frontend serves the app from this same
	// origin. A cross-origin frontend would have to weaken this to Lax.

	if value == "" {
		c.MaxAge = -1 // delete now
		return c
	}

	// Persistent, not a browser-session cookie: closing the browser keeps you
	// logged in, and the server's expires_at is what actually ends it.
	c.MaxAge = int(time.Until(expiresAt).Seconds())

	return c
}

/*
┌─ auth ──────────────────────────────────────────
│  resolves either credential to one identity
├─ in ────────────────────────────────────────────
│      ctx        context.Context
│      header     string      Authorization value, may be empty
│      cookie     string      session cookie value, may be empty
├─ out ───────────────────────────────────────────
│      Identity
│      error                  always ErrInvalidToken
├─ example ───────────────────────────────────────
│      "Bearer lam_…", ""  →  Identity with Scopes
*/

func (s *Service) Authenticate(ctx context.Context, header, cookie string) (Identity, error) {
	// Header first: a script sending a token explicitly means it, and a stale
	// cookie in the same request should not quietly win.
	if token, ok := BearerToken(header); ok {
		return s.Validate(ctx, token)
	}

	if cookie != "" {
		return s.ValidateSession(ctx, cookie)
	}

	return Identity{}, ErrInvalidToken
}

// hashSessionID is what the session table stores. SHA-256, not bcrypt: the id
// is 32 random bytes, so there is nothing to guess and a slow hash would only
// add its cost to every page load. See 0028_session.sql.
func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

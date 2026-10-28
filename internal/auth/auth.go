/*
╔═ auth.go ═════════════════════════════════════════════════════════════════════════════
║  auth · api token validation
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Scope              string
║      Identity           struct
║      Service            struct
║      ErrInvalidToken    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      nothing yet  →  auth middleware lands with LAM-56
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package auth is the only code in this module that reads api_token.
//
// API Design notes section 5 allows the per-request Postgres lookup only while it stays
// behind one entry point, because that is where a cache can later go without touching a
// single endpoint. sqlguard fails the build if another package reaches around it.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// One error for every failure, so no caller can enumerate prefixes or learn that an
// expired token was once real.
var ErrInvalidToken = errors.New("auth: invalid token")

// ErrTokenNotFound is Revoke's answer for a token that is absent, another account's, or
// already revoked - one error, so the three cannot be told apart.
var ErrTokenNotFound = errors.New("auth: token not found")

// A token's last_used_at moves at most this often, so a busy script does not turn every
// request into a write.
const lastUsedInterval = time.Minute

const (
	tokenPrefixBytes = 6  // 10 base32 chars, the lookup key, not secret
	tokenSecretBytes = 32 // 256 bits, all of the entropy
	tokenScheme      = "lam"
)

// A real bcrypt hash of a value nothing presents, so an unknown prefix costs the same
// comparison as a known one. Random at init, so it is not recognisable in the binary.
var dummyHash []byte

/*
┏━ Scope ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one permission a token carries, as resource:action
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      string
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Issue · Validate               no registry until LAM-57
*/

type Scope string

/*
┏━ Identity ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  who a valid token belongs to and what it may do
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      AccountID    string
┃      TokenID      string           empty for a session
┃      SessionID    string           empty for a token
┃      Scopes       []Scope          nil for a session
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Validate                      one per request
*/

type Identity struct {
	AccountID string
	TokenID   string
	SessionID string
	Scopes    []Scope
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of api_token, reads and writes
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool         *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Validate(ctx, string)                   →  Identity, error
┃      Issue(ctx, string, string, []Scope, *time.Time)  →  string, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                    one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

/*
┌─ auth ──────────────────────────────────────────
│  builds the one reader of api_token
├─ in ────────────────────────────────────────────
│      pool     *pgxpool.Pool
├─ out ───────────────────────────────────────────
│      *Service
*/

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

/*
┌─ auth ──────────────────────────────────────────
│  the single entry point a cache would later sit in
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      presented    string            scheme_prefix_secret
├─ out ───────────────────────────────────────────
│      Identity                       account and scopes
│      error                          always ErrInvalidToken
├─ example ───────────────────────────────────────
│      lam_k3f2nq7x_h9w…  →  Identity{AccountID: "8b1c…"}
*/

func (s *Service) Validate(ctx context.Context, presented string) (Identity, error) {
	prefix, secret, ok := splitToken(presented)
	if !ok {
		// Malformed reveals no prefix, so it need not pay the bcrypt cost.
		return Identity{}, ErrInvalidToken
	}

	var (
		id        string
		accountID string
		hash      string
		scopes    []string
		expiresAt *time.Time
		revokedAt *time.Time
	)

	err := s.pool.QueryRow(ctx,
		`SELECT id, account_id, token_hash, scopes, expires_at, revoked_at
		   FROM api_token
		  WHERE token_prefix = $1`, prefix,
	).Scan(&id, &accountID, &hash, &scopes, &expiresAt, &revokedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(secret))
		return Identity{}, ErrInvalidToken
	case err != nil:
		// A database fault is not an invalid token; a caller retrying against a 401
		// forever is worse than a 500.
		return Identity{}, fmt.Errorf("auth: look up token: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)); err != nil {
		return Identity{}, ErrInvalidToken
	}

	// In Go, not in the WHERE clause: a SQL predicate would skip the comparison above
	// and hand back the timing difference dummyHash exists to remove. Revocation is
	// checked here for the same reason.
	if revokedAt != nil || (expiresAt != nil && !expiresAt.After(time.Now())) {
		return Identity{}, ErrInvalidToken
	}

	// Best effort: a failed write must not fail a request whose credential is good.
	// The predicate makes it at most one write per token per interval.
	_, _ = s.pool.Exec(ctx,
		`UPDATE api_token SET last_used_at = now()
		  WHERE id = $1
		    AND (last_used_at IS NULL OR last_used_at < now() - $2::interval)`,
		id, lastUsedInterval.String())

	return Identity{AccountID: accountID, TokenID: id, Scopes: toScopes(scopes)}, nil
}

/*
┌─ auth ──────────────────────────────────────────
│  mints a token, returning the only copy of its secret
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      label        string            shown in place of the hash
│      scopes       []Scope           nil can do nothing
│      expiresAt    *time.Time        nil never expires
├─ out ───────────────────────────────────────────
│      string                         scheme_prefix_secret
│      error
├─ example ───────────────────────────────────────
│      "CI deploy key"  →  lam_k3f2nq7x_h9w4…52 chars
*/

func (s *Service) Issue(ctx context.Context, accountID, label string, scopes []Scope, expiresAt *time.Time) (string, error) {
	_, secret, err := s.Create(ctx, accountID, label, scopes, expiresAt)

	return secret, err
}

/*
┏━ Token ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what may be shown about a token after it is issued
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID, Label    string
┃      Scopes       []Scope          never nil
┃      CreatedAt    time.Time
┃      ExpiresAt    *time.Time       nil never expires
┃      LastUsedAt   *time.Time       nil never used
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Create · List                 no hash, no secret, no prefix
*/

type Token struct {
	ID         string
	Label      string
	Scopes     []Scope
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

/*
┌─ auth ──────────────────────────────────────────
│  Issue, keeping the row's metadata for the caller
├─ in ────────────────────────────────────────────
│      as Issue
├─ out ───────────────────────────────────────────
│      Token                          id, label, scopes, dates
│      string                         scheme_prefix_secret, once
│      error
*/

func (s *Service) Create(ctx context.Context, accountID, label string, scopes []Scope, expiresAt *time.Time) (Token, string, error) {
	prefix, err := randomString(tokenPrefixBytes)
	if err != nil {
		return Token{}, "", fmt.Errorf("auth: generate prefix: %w", err)
	}

	secret, err := randomString(tokenSecretBytes)
	if err != nil {
		return Token{}, "", fmt.Errorf("auth: generate secret: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return Token{}, "", fmt.Errorf("auth: hash secret: %w", err)
	}

	names := make([]string, len(scopes))
	for i, sc := range scopes {
		names[i] = string(sc)
	}

	t := Token{Label: label, Scopes: toScopes(names), ExpiresAt: expiresAt}
	if t.Scopes == nil {
		t.Scopes = []Scope{}
	}

	if err := s.pool.QueryRow(ctx,
		`INSERT INTO api_token (account_id, label, token_prefix, token_hash, scopes, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		accountID, label, prefix, string(hash), names, expiresAt,
	).Scan(&t.ID, &t.CreatedAt); err != nil {
		return Token{}, "", fmt.Errorf("auth: insert token: %w", err)
	}

	return t, tokenScheme + "_" + prefix + "_" + secret, nil
}

/*
┌─ auth ──────────────────────────────────────────
│  an account's active tokens, newest first
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      []Token                        revoked and expired are left out
│      error
*/

func (s *Service) List(ctx context.Context, accountID string) ([]Token, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, label, scopes, created_at, expires_at, last_used_at
		   FROM api_token
		  WHERE account_id = $1
		    AND revoked_at IS NULL
		    AND (expires_at IS NULL OR expires_at > now())
		  ORDER BY created_at DESC, id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("auth: list tokens: %w", err)
	}
	defer rows.Close()

	out := []Token{}
	for rows.Next() {
		var (
			t      Token
			scopes []string
		)
		if err := rows.Scan(&t.ID, &t.Label, &scopes, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, fmt.Errorf("auth: scan token: %w", err)
		}
		t.Scopes = toScopes(scopes)
		if t.Scopes == nil {
			t.Scopes = []Scope{}
		}
		out = append(out, t)
	}

	return out, rows.Err()
}

/*
┌─ auth ──────────────────────────────────────────
│  revokes a token, keeping its row
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      tokenID      string
├─ out ───────────────────────────────────────────
│      error        ErrTokenNotFound for absent, another account's, or already revoked
*/

func (s *Service) Revoke(ctx context.Context, accountID, tokenID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_token SET revoked_at = now(), updated_at = now()
		  WHERE id = $1 AND account_id = $2 AND revoked_at IS NULL`,
		tokenID, accountID)
	if err != nil {
		return fmt.Errorf("auth: revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}

	return nil
}

/*
┌─ auth ──────────────────────────────────────────
│  pulls the credential out of an Authorization value
├─ in ────────────────────────────────────────────
│      header    string      takes the value, not a *http.Request
├─ out ───────────────────────────────────────────
│      string                the token
│      bool                  false when absent or not Bearer
├─ example ───────────────────────────────────────
│      "Bearer lam_a_b"  →  "lam_a_b", true
*/

func BearerToken(header string) (string, bool) {
	const prefix = "Bearer "

	// Scheme is case-insensitive per RFC 7235; the token is not.
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}

	return token, true
}

/*
┌─ auth ──────────────────────────────────────────
│  splits a presented token into lookup key and secret
├─ in ────────────────────────────────────────────
│      presented    string
├─ out ───────────────────────────────────────────
│      prefix       string
│      secret       string
│      ok           bool      false on any shape but three parts
├─ example ───────────────────────────────────────
│      "lam_k3f2nq7x_h9w4"  →  "k3f2nq7x", "h9w4", true
*/

func splitToken(presented string) (prefix, secret string, ok bool) {
	parts := strings.Split(presented, "_")
	if len(parts) != 3 || parts[0] != tokenScheme || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}

	return parts[1], parts[2], true
}

/*
┌─ auth ──────────────────────────────────────────
│  random bytes as base32, whose alphabet excludes "_"
├─ in ────────────────────────────────────────────
│      n        int       bytes of entropy
├─ out ───────────────────────────────────────────
│      string             unpadded lowercase a-z2-7
│      error
├─ example ───────────────────────────────────────
│      6  →  "k3f2nq7x2a"        base64url would emit "_"
*/

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func toScopes(names []string) []Scope {
	if len(names) == 0 {
		return nil
	}

	out := make([]Scope, len(names))
	for i, n := range names {
		out[i] = Scope(n)
	}

	return out
}

func init() {
	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		panic("auth: no entropy for the dummy hash: " + err.Error())
	}

	h, err := bcrypt.GenerateFromPassword(secret, bcrypt.DefaultCost)
	if err != nil {
		panic("auth: cannot build the dummy hash: " + err.Error())
	}
	dummyHash = h
}

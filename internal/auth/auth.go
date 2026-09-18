// Package auth is the only code in this module that reads api_token.
//
// That is the whole point of it, and it comes straight from API Design notes
// section 5. Token validation is a direct Postgres lookup today, and that was
// only a safe thing to choose because a cache can be added later behind a
// single entry point - "adding a caching layer later is purely an internal
// change to that one function and requires no changes to any endpoint that
// uses it." An endpoint that queries api_token itself takes that option away
// permanently, and nothing about the endpoint would look wrong.
//
// So: one Service, one exported Validate, and a boundary test that fails the
// build if any other package names the table in SQL. Same three-layer
// approach as internal/document, for the same reason - see
// docs/adr/0001-write-path-enforcement.md.
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

// ErrInvalidToken is the only failure Validate reports.
//
// Deliberately one error for every reason a token does not work: unknown
// prefix, wrong secret, expired, malformed. A caller that can tell those apart
// can enumerate which prefixes exist, and an expired-versus-unknown
// distinction tells an attacker a token was real.
var ErrInvalidToken = errors.New("auth: invalid token")

// Scope is a permission a token carries.
//
// The format is resource:action - "tickets:read", "documents:write". The
// vocabulary itself is deliberately not defined here: no endpoint requires a
// scope yet, and naming a set now would be inventing structure nothing
// consumes. Whichever ticket adds the first endpoint that checks a scope owns
// the registry, and LAM-57 is the natural first.
//
// Validate returns scopes and never checks them. Which scope an endpoint needs
// is the endpoint's business; proving the caller holds a token is this
// package's.
type Scope string

// Identity is who a valid token belongs to and what it may do.
type Identity struct {
	AccountID string
	TokenID   string
	Scopes    []Scope
}

// tokenPrefixBytes and tokenSecretBytes size the two halves.
//
// The prefix only has to be unique enough to be a lookup key, and it is not
// secret - 6 bytes is 10 base32 characters, the same order as GitHub's. The
// secret carries all the entropy: 32 bytes is 256 bits, so guessing it is not
// a threat model, and the slow hash below is defence against a stolen database
// rather than against online guessing.
const (
	tokenPrefixBytes = 6
	tokenSecretBytes = 32

	// tokenScheme prefixes every token this service mints, so a leaked string
	// is recognisable as a LaminarFlow credential in a log or a paste.
	tokenScheme = "lam"
)

// dummyHash is a real bcrypt hash of a value nothing will ever present.
//
// Validate compares against it when the prefix matches no row, so an unknown
// prefix costs the same bcrypt comparison as a known one. Without this, a
// response for an unknown prefix returns in about a millisecond while a known
// one takes the full bcrypt cost, and the difference is a reliable oracle for
// which prefixes exist - measurable over a network, and the reason step 4 of
// LAM-55 asks for it.
//
// Generated at init from a random value rather than hardcoded, so it is not a
// constant an attacker can recognise in the binary.
var dummyHash []byte

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

// Service owns api_token.
//
// pool is unexported: no caller outside this package can reach through a
// Service to the database, which is mechanism 1 of the three ADR 0001
// describes.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service reading and writing api_token through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Validate is the single entry point API Design notes section 5 requires.
//
// Presented token in, Identity out, or ErrInvalidToken. A cache would go
// inside this function - check the cache, fall back to Postgres - and no
// caller would change. That property is the reason the direct lookup was an
// acceptable starting point, so it is worth more than any micro-optimisation
// that would break it.
//
// Exactly one bcrypt comparison happens per call, whether or not the prefix
// exists. That cost is deliberate and is what migrations/0008_api_token.sql
// committed to when it split the credential; see dummyHash for why the
// unknown-prefix path pays it too.
func (s *Service) Validate(ctx context.Context, presented string) (Identity, error) {
	prefix, secret, ok := splitToken(presented)
	if !ok {
		// A malformed token reveals nothing about which prefixes exist, so
		// this returns without paying the bcrypt cost.
		return Identity{}, ErrInvalidToken
	}

	var (
		id        string
		accountID string
		hash      string
		scopes    []string
		expiresAt *time.Time
	)

	err := s.pool.QueryRow(ctx,
		`SELECT id, account_id, token_hash, scopes, expires_at
		   FROM api_token
		  WHERE token_prefix = $1`, prefix,
	).Scan(&id, &accountID, &hash, &scopes, &expiresAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Pay the same cost as a hit, then fail. See dummyHash.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(secret))
		return Identity{}, ErrInvalidToken
	case err != nil:
		// A database fault is not an invalid token, and must not be reported
		// as one - a caller retrying forever against a 401 is worse than a 500.
		return Identity{}, fmt.Errorf("auth: look up token: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(secret)); err != nil {
		return Identity{}, ErrInvalidToken
	}

	// Checked in Go rather than in the WHERE clause on purpose. An expiry
	// predicate in SQL would make an expired token indistinguishable from an
	// unknown prefix at the query, which sounds like the safe direction but
	// skips the bcrypt comparison - handing back the timing difference
	// dummyHash exists to remove.
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return Identity{}, ErrInvalidToken
	}

	return Identity{AccountID: accountID, TokenID: id, Scopes: toScopes(scopes)}, nil
}

// Issue mints a token for an account and returns it once.
//
// The returned string is the only time the secret half exists outside the
// caller's hand: the row stores a hash of it, so nothing can recover it later.
// A token management screen shows label and prefix, never this.
//
// Issuance lives here rather than in a sibling ticket because the token format
// is the coupling. splitToken and Issue have to agree about the scheme, the
// separator and the two halves, and a format defined in two packages is a
// format that drifts. There is deliberately no HTTP endpoint - creating a
// token requires already being authenticated, so that belongs with the session
// work in LAM-56.
func (s *Service) Issue(ctx context.Context, accountID, label string, scopes []Scope, expiresAt *time.Time) (string, error) {
	prefix, err := randomString(tokenPrefixBytes)
	if err != nil {
		return "", fmt.Errorf("auth: generate prefix: %w", err)
	}

	secret, err := randomString(tokenSecretBytes)
	if err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash secret: %w", err)
	}

	// scopes defaults to '{}' in the schema and a scopeless token can do
	// nothing, which is the direction an auth default should fail in. Passing
	// an empty slice preserves that rather than substituting a wildcard.
	names := make([]string, len(scopes))
	for i, sc := range scopes {
		names[i] = string(sc)
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO api_token (account_id, label, token_prefix, token_hash, scopes, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		accountID, label, prefix, string(hash), names, expiresAt,
	); err != nil {
		return "", fmt.Errorf("auth: insert token: %w", err)
	}

	return tokenScheme + "_" + prefix + "_" + secret, nil
}

// BearerToken pulls the credential out of an Authorization header value.
//
// Authorization: Bearer <token> is the answer LAM-55 step 3 asked for, chosen
// because it is what every client library, proxy and log redaction rule
// already expects. A bespoke header would work and would need a reason.
//
// Takes the header value rather than an *http.Request so this package stays
// free of net/http: a validator that imports the web framework is a validator
// that is awkward to call from anything else.
func BearerToken(header string) (string, bool) {
	const prefix = "Bearer "

	// Scheme comparison is case-insensitive per RFC 7235; the token is not.
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}

	return token, true
}

// splitToken parses a presented token into its lookup key and its secret.
func splitToken(presented string) (prefix, secret string, ok bool) {
	parts := strings.Split(presented, "_")
	if len(parts) != 3 || parts[0] != tokenScheme || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}

	return parts[1], parts[2], true
}

// randomString returns n random bytes as unpadded lowercase base32.
//
// The encoding is load-bearing, and base64url was wrong here. Its alphabet
// includes "_" and "-", so a secret could contain the separator splitToken
// parses on - which it promptly did, producing tokens that could be issued and
// never validated. base32's alphabet is a-z2-7 after lowercasing: no
// separator, and safe in a header, a URL and a shell argument unquoted.
//
// Nothing ever decodes these - the value is compared as a string and hashed as
// bytes - so the encoding is chosen purely for its alphabet.
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

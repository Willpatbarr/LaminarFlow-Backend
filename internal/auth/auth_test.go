package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// An endpoint reading api_token itself removes the caching option section 5 depends
// on, and would look fine doing it. Go cannot express the rule; this can.
func TestNoAPITokenSQLOutsideThisPackage(t *testing.T) {
	sqlguard.AssertOwned(t, "auth", "api_token", "session")
}

// The round trip: Issue returns the only copy of the secret, Validate accepts it.
func TestIssuedTokenValidates(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	token, err := svc.Issue(ctx, accountID, "CI deploy key", []Scope{"tickets:read"}, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	id, err := svc.Validate(ctx, token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	if id.AccountID != accountID {
		t.Errorf("account = %q, want %q", id.AccountID, accountID)
	}
	if len(id.Scopes) != 1 || id.Scopes[0] != "tickets:read" {
		t.Errorf("scopes = %v, want [tickets:read]", id.Scopes)
	}
	if id.TokenID == "" {
		t.Error("token id is empty, so a caller cannot tell which token was used")
	}
}

// 0008's column comment promises a hash and never the token. This is that promise.
func TestTheStoredRowDoesNotContainTheSecret(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	token, err := svc.Issue(ctx, accountID, "CI deploy key", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	_, secret, ok := splitToken(token)
	if !ok {
		t.Fatalf("issued token %q does not parse", token)
	}

	var hash string
	err = svc.pool.QueryRow(ctx,
		`SELECT token_hash FROM api_token WHERE account_id = $1`, accountID).Scan(&hash)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}

	if strings.Contains(hash, secret) {
		t.Error("token_hash contains the secret verbatim")
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("token_hash = %q, want a bcrypt hash - 0008 requires a slow hash", hash)
	}
}

// One error for every failure, or a caller can enumerate prefixes and learn that an
// expired token was once real.
func TestEveryFailureIsIndistinguishable(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	valid, err := svc.Issue(ctx, accountID, "valid", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	prefix, _, ok := splitToken(valid)
	if !ok {
		t.Fatalf("issued token %q does not parse", valid)
	}

	past := time.Now().Add(-time.Hour)
	expired, err := svc.Issue(ctx, accountID, "expired", nil, &past)
	if err != nil {
		t.Fatalf("issue expired: %v", err)
	}

	otherSecret, err := randomString(tokenSecretBytes)
	if err != nil {
		t.Fatalf("random: %v", err)
	}

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"no scheme", "abc_def"},
		{"wrong scheme", "gh_" + prefix + "_" + otherSecret},
		{"missing secret", tokenScheme + "_" + prefix + "_"},
		{"unknown prefix", tokenScheme + "_zzzzzzzz_" + otherSecret},
		{"right prefix, wrong secret", tokenScheme + "_" + prefix + "_" + otherSecret},
		{"expired", expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Validate(ctx, tc.token)
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// The expiry check must not reject everything that merely carries a date.
func TestAnUnexpiredExpiryStillValidates(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	token, err := svc.Issue(ctx, accountID, "expires later", nil, &future)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if _, err := svc.Validate(ctx, token); err != nil {
		t.Errorf("validate: %v, want success", err)
	}
}

// Scopeless is legal and can do nothing - the direction an auth default should fail.
func TestAScopelessTokenValidatesAndCarriesNoScopes(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	token, err := svc.Issue(ctx, accountID, "no scopes", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	id, err := svc.Validate(ctx, token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(id.Scopes) != 0 {
		t.Errorf("scopes = %v, want none", id.Scopes)
	}
}

// The prefix is the UNIQUE lookup key, so a collision is a 23505, not a wrong row.
func TestTwoTokensGetDifferentPrefixes(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	first, err := svc.Issue(ctx, accountID, "one", nil, nil)
	if err != nil {
		t.Fatalf("issue first: %v", err)
	}
	second, err := svc.Issue(ctx, accountID, "two", nil, nil)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}

	p1, s1, ok1 := splitToken(first)
	p2, s2, ok2 := splitToken(second)
	if !ok1 || !ok2 {
		t.Fatalf("issued tokens do not parse: %q (%v), %q (%v)", first, ok1, second, ok2)
	}

	if p1 == p2 {
		t.Errorf("both tokens got prefix %q", p1)
	}
	if s1 == s2 {
		t.Error("both tokens got the same secret")
	}
}

// Proves the lookup keys on prefix rather than returning whatever row comes first.
func TestEachTokenValidatesToItsOwnRow(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	first, err := svc.Issue(ctx, accountID, "one", []Scope{"a:read"}, nil)
	if err != nil {
		t.Fatalf("issue first: %v", err)
	}
	second, err := svc.Issue(ctx, accountID, "two", []Scope{"b:write"}, nil)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}

	one, err := svc.Validate(ctx, first)
	if err != nil {
		t.Fatalf("validate first: %v", err)
	}
	two, err := svc.Validate(ctx, second)
	if err != nil {
		t.Fatalf("validate second: %v", err)
	}

	if one.TokenID == two.TokenID {
		t.Error("two tokens validated to the same row")
	}
	if len(one.Scopes) != 1 || one.Scopes[0] != "a:read" {
		t.Errorf("first scopes = %v, want [a:read]", one.Scopes)
	}
	if len(two.Scopes) != 1 || two.Scopes[0] != "b:write" {
		t.Errorf("second scopes = %v, want [b:write]", two.Scopes)
	}
}

// 0008 calls this CASCADE a security property: a token outliving its account is an
// open door.
func TestDeletingTheAccountInvalidatesItsTokens(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	token, err := svc.Issue(ctx, accountID, "doomed", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := svc.Validate(ctx, token); err != nil {
		t.Fatalf("validate before delete: %v", err)
	}

	if _, err := svc.pool.Exec(ctx, `DELETE FROM account WHERE id = $1`, accountID); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	if _, err := svc.Validate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken after the account is gone", err)
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
		ok                 bool
	}{
		{"typical", "Bearer lam_abc_def", "lam_abc_def", true},
		{"lowercase scheme", "bearer lam_abc_def", "lam_abc_def", true},
		{"mixed case scheme", "BeArEr lam_abc_def", "lam_abc_def", true},
		{"trailing space", "Bearer lam_abc_def  ", "lam_abc_def", true},
		{"empty", "", "", false},
		{"scheme only", "Bearer ", "", false},
		{"scheme with no space", "Bearerlam_abc", "", false},
		{"wrong scheme", "Basic lam_abc_def", "", false},
		{"bare token", "lam_abc_def", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BearerToken(tc.header)
			if ok != tc.ok || got != tc.want {
				t.Errorf("BearerToken(%q) = %q, %v; want %q, %v",
					tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Changing the scheme invalidates every token already issued: a migration, not an edit.
func TestIssuedTokensCarryTheScheme(t *testing.T) {
	svc, accountID := newTestService(t)
	ctx := context.Background()

	token, err := svc.Issue(ctx, accountID, "scheme check", nil, nil)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if !strings.HasPrefix(token, tokenScheme+"_") {
		t.Errorf("token = %q, want the %q_ scheme so a leaked string is recognisable",
			token, tokenScheme)
	}
}

// base64url's alphabet contains "_", the separator splitToken parses on - tokens
// issued fine and never validated. Many samples: the old encoding only broke half
// the time.
func TestTheTokenAlphabetExcludesTheSeparator(t *testing.T) {
	for i := 0; i < 200; i++ {
		s, err := randomString(tokenSecretBytes)
		if err != nil {
			t.Fatalf("randomString: %v", err)
		}
		if strings.Contains(s, "_") {
			t.Fatalf("sample %d = %q contains the separator, so a token carrying "+
				"it cannot be parsed back", i, s)
		}
	}
}

// Whatever Issue composes, splitToken must take apart - failing here rather than as
// four confusing database-test failures.
func TestIssueAndSplitTokenAgreeOnTheFormat(t *testing.T) {
	for i := 0; i < 200; i++ {
		prefix, err := randomString(tokenPrefixBytes)
		if err != nil {
			t.Fatalf("prefix: %v", err)
		}
		secret, err := randomString(tokenSecretBytes)
		if err != nil {
			t.Fatalf("secret: %v", err)
		}

		gotPrefix, gotSecret, ok := splitToken(tokenScheme + "_" + prefix + "_" + secret)
		if !ok {
			t.Fatalf("sample %d does not parse: prefix %q secret %q", i, prefix, secret)
		}
		if gotPrefix != prefix || gotSecret != secret {
			t.Fatalf("sample %d round-tripped to prefix %q secret %q, want %q and %q",
				i, gotPrefix, gotSecret, prefix, secret)
		}
	}
}

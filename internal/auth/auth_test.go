package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sqlguard"
)

// The rule this package exists to hold. API Design notes section 5 permits the
// direct Postgres lookup only because a cache can be added behind one entry
// point later; an endpoint that reads api_token itself removes that option and
// nothing about the endpoint would look wrong.
//
// Go cannot express this - encapsulation is package-scoped, so an unexported
// pool stops a caller reaching through a Service but not a new package calling
// db.Connect. See docs/adr/0001-write-path-enforcement.md.
func TestNoAPITokenSQLOutsideThisPackage(t *testing.T) {
	sqlguard.AssertOwned(t, "auth", "api_token")
}

// The round trip. Issue returns the only copy of the secret; Validate accepts
// it and reports who it belongs to.
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

// The secret must not be recoverable from the row. 0008's column comment
// promises the stored value is a hash and never the token, and this is the
// assertion behind that promise.
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

// Every way a token can fail returns the same error. A caller that can tell
// these apart can enumerate which prefixes exist, and can learn that an
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

// A token expiring in the future still works, so the expiry check is not
// rejecting everything that carries a date.
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

// A scopeless token is legal and does nothing, which is the direction 0008
// chose for the default and the direction an auth default should fail in.
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

// Two tokens must never share a prefix: the prefix is the lookup key and
// carries a UNIQUE constraint, so a collision would surface as a 23505 at
// issue time rather than as a wrong lookup.
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

// Each token validates to its own row, which is what proves the lookup keys on
// prefix rather than returning whatever row comes first.
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

// Deleting the account deletes its tokens. 0008 calls the CASCADE a security
// property rather than a convenience - a token outliving its account is an
// open door - so it is asserted through this package rather than left to the
// schema tests alone.
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

// The scheme is part of the format, so it is pinned. Changing it invalidates
// every token already issued, which is a migration rather than an edit.
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

// The bug this pins: the first version of randomString used base64url, whose
// alphabet contains "_" - the separator splitToken parses on. Tokens were
// issued successfully and then never validated, because Split produced five
// parts instead of three.
//
// Asserted over many samples rather than one, since the old encoding only
// produced a separator in roughly half of them and a single draw could pass.
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

// The round trip at the format level, independent of the database: whatever
// Issue composes, splitToken must take apart. Cheap, and it fails in one place
// rather than as four confusing database-test failures.
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

package auth

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordLengthIsCountedInCharactersWithAByteCeiling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		want     error
	}{
		{"nine characters", "123456789", ErrPasswordTooShort},
		{"empty", "", ErrPasswordTooShort},
		{"ten characters", "1234567890", nil},
		// Twenty bytes, ten characters: a floor in bytes would let a nine-character
		// password in any non-ASCII script through.
		{"ten multi-byte characters", strings.Repeat("é", 10), nil},
		{"nine multi-byte characters", strings.Repeat("é", 9), ErrPasswordTooShort},
		{"72 bytes", strings.Repeat("a", 72), nil},
		{"73 bytes", strings.Repeat("a", 73), ErrPasswordTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidatePassword(tc.password); !errors.Is(got, tc.want) {
				t.Errorf("ValidatePassword = %v, want %v", got, tc.want)
			}
		})
	}
}

// What register stores has to be what LogIn compares against, or sign-up produces
// accounts nobody can log in to.
func TestHashPasswordProducesWhatLogInCompares(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if hash == "correct horse battery" {
		t.Fatal("the password was stored as plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse battery")); err != nil {
		t.Errorf("hash does not verify: %v", err)
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != bcrypt.DefaultCost {
		t.Errorf("cost = %d, want %d, the cost dummyHash is built at", cost, bcrypt.DefaultCost)
	}
}

var permissionName = regexp.MustCompile(`^[a-z]+:[a-z]+$`)

func TestPermissionsAreAClosedSetOfResourceActionNames(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range Permissions(Identity{}) {
		if !permissionName.MatchString(name) {
			t.Errorf("%q is not resource:action", name)
		}
		if seen[name] {
			t.Errorf("%q is listed twice", name)
		}
		seen[name] = true

		p, ok := ParsePermission(name)
		if !ok || p.String() != name {
			t.Errorf("%q does not round-trip through ParsePermission", name)
		}
	}

	if !seen["ticket:write"] {
		t.Error("ticket:write is missing from the set")
	}
	if _, ok := ParsePermission("ticket:wirte"); ok {
		t.Error("a typo parsed as a permission")
	}
}

func TestPermissionsReturnsAFreshNonNilSlice(t *testing.T) {
	first := Permissions(Identity{})
	if first == nil {
		t.Fatal("nil would marshal as null, not a list")
	}

	first[0] = "tampered"
	if Permissions(Identity{})[0] == "tampered" {
		t.Error("a caller could rewrite the registry through the returned slice")
	}
}

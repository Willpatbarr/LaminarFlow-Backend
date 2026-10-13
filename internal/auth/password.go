/*
╔═ password.go ═════════════════════════════════════════════════════════════════════════
║  auth · password policy and hashing
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      MinPasswordLength    const
║      MaxPasswordBytes     const
║      ErrPasswordTooShort  error
║      ErrPasswordTooLong   error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  register
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinPasswordLength is counted in characters, not bytes, so a passphrase in any
	// script is held to the same bar. Ten is a floor against the obvious, not a
	// strength meter: length is the one property that can be checked without
	// pretending to know what a guessable password looks like.
	MinPasswordLength = 10

	// MaxPasswordBytes is bcrypt's own limit. Past 72 bytes it either truncates
	// silently or, in current x/crypto, refuses - and a password whose tail is
	// ignored is worse than one that is rejected, because the user believes the
	// tail protects them.
	MaxPasswordBytes = 72
)

var (
	ErrPasswordTooShort = errors.New("auth: password is too short")
	ErrPasswordTooLong  = errors.New("auth: password is too long")
)

/*
┌─ auth ──────────────────────────────────────────
│  checks a new password against the policy
├─ in ────────────────────────────────────────────
│      password    string
├─ out ───────────────────────────────────────────
│      error       ErrPasswordTooShort · ErrPasswordTooLong
├─ example ───────────────────────────────────────
│      "short"  →  ErrPasswordTooShort
*/

func ValidatePassword(password string) error {
	switch {
	case utf8.RuneCountInString(password) < MinPasswordLength:
		return ErrPasswordTooShort
	case len(password) > MaxPasswordBytes:
		return ErrPasswordTooLong
	}

	return nil
}

/*
┌─ auth ──────────────────────────────────────────
│  the one place a password becomes a stored hash
├─ in ────────────────────────────────────────────
│      password    string      already validated
├─ out ───────────────────────────────────────────
│      string                  bcrypt, cost 10
│      error
*/

func HashPassword(password string) (string, error) {
	// DefaultCost on purpose: it is the cost dummyHash is built at, so a login
	// against a missing account and one against this hash take the same time.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}

	return string(hash), nil
}

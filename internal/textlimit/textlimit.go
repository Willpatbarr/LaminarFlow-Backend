/*
╔═ textlimit.go ════════════════════════════════════════════════════════════════════════
║  text limits · one counting rule for every user-written string
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ErrBlank        error
║      ErrTooLong      error
║      FieldError      struct
║      Required        func
║      Optional        func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      ticket · team · project  →  Create and Update, before any SQL
║      internal/api             →  FieldError becomes a 422 at body.<field>
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package textlimit is the one place that decides how long a string is.
//
// Characters, not bytes - the distinction internal/auth/password.go already makes for
// the password floor. A limit in bytes turns "200" into 66 Japanese characters, and a
// limit that is different for different alphabets is not a limit anyone can explain.
// A character here is a Unicode code point, which is what huma's minLength and
// maxLength count too (utf8.RuneCountInString, huma v2.39.1 validate.go), so the
// schema in the OpenAPI document and the check in the service agree on every input.
// It is not a grapheme cluster: a flag emoji is two code points and counts as two.
//
// The limits themselves live beside each resource (ticket.MaxTitleLength and so on);
// this package knows no resource, only how to count and what blank means.
package textlimit

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	// ErrBlank: nothing but whitespace, or nothing at all.
	ErrBlank = errors.New("must not be blank")

	// ErrTooLong: more characters than the field allows.
	ErrTooLong = errors.New("is too long")
)

/*
┏━ FieldError ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  a string the service refused, and which one
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      error
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Field     string     the JSON name: title, description, name
┃      Max       int        characters allowed
┃      Reason    error      ErrBlank · ErrTooLong
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Required · Optional           one per refused string
*/

// FieldError carries the field name so the API can place the failure at body.<field>
// without parsing a message. Field is the name on the wire, which is also the name in
// the service's params; they agree for every field that uses this.
type FieldError struct {
	Field  string
	Max    int
	Reason error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s %s", e.Field, e.Message())
}

func (e *FieldError) Unwrap() error { return e.Reason }

// Message is the part a person reads: what is wrong, without the field name.
func (e *FieldError) Message() string {
	if errors.Is(e.Reason, ErrTooLong) {
		return fmt.Sprintf("must be at most %d characters", e.Max)
	}

	return e.Reason.Error()
}

/*
┌─ textlimit ─────────────────────────────────────
│  checks a string that must say something
├─ in ────────────────────────────────────────────
│      field    string     the JSON name
│      s        string
│      limit    int        characters
├─ out ───────────────────────────────────────────
│      error               nil · *FieldError
├─ example ───────────────────────────────────────
│      "title", "   ", 200  →  ErrBlank
*/

func Required(field, s string, limit int) error {
	if strings.TrimSpace(s) == "" {
		return &FieldError{Field: field, Max: limit, Reason: ErrBlank}
	}

	return Optional(field, s, limit)
}

/*
┌─ textlimit ─────────────────────────────────────
│  checks a string that may be empty
├─ in ────────────────────────────────────────────
│      field    string     the JSON name
│      s        string
│      limit    int        characters
├─ out ───────────────────────────────────────────
│      error               nil · *FieldError
├─ example ───────────────────────────────────────
│      200 × "あ", 200  →  nil
*/

func Optional(field, s string, limit int) error {
	if utf8.RuneCountInString(s) > limit {
		return &FieldError{Field: field, Max: limit, Reason: ErrTooLong}
	}

	return nil
}

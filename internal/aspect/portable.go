/*
╔═ portable.go ═════════════════════════════════════════════════════════════════════════
║  aspect · the portable JSON form of an aspect type (LAM-110)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      FormatVersion      const
║      Portable           struct
║      PortableField      struct
║      FormatError        struct
║      Export             method
║      Import             method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  GET /aspect-types/{id}/export · POST /aspect-types/import
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package aspect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
)

// FormatVersion is the only portable format there is so far. A reader refuses any other
// number rather than guessing, and a change that an old reader would misread bumps it.
// The format is documented in docs/aspect-type-format.md.
const FormatVersion = 1

// Limits of the portable format. Names and labels have no limit on the editor's own
// endpoints, so these bind import only: an import is the one place a whole type arrives
// at once from a file nobody here wrote. A type already stored with a longer name still
// exports; it is the import of it that is refused, naming the field to shorten.
const (
	MaxNameLength   = 100
	MaxLabelLength  = 100
	MaxImportFields = 100
	MaxPosition     = 1_000_000
)

/*
┏━ Portable ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  an aspect type as it travels between teams
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      FormatVersion    int
┃      Name             string
┃      Fields           []PortableField    in position order
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Export · the API's import body
*/

// Portable carries no id of any kind - not the type's, not a field's, not the team's.
// A field id is the key of document.body, so a copy of it in another team would be a
// key into nothing; import mints fresh ones.
type Portable struct {
	FormatVersion int
	Name          string
	Fields        []PortableField
}

// PortableField is a label and where it sits.
type PortableField struct {
	Label    string
	Position int
}

// FormatError is a file the format refuses, and which part of it. Field is the path in
// the file: format_version, fields, fields[3].position.
type FormatError struct {
	Field       string
	Message     string
	Unsupported bool // format_version names a version this server does not read
}

func (e *FormatError) Error() string { return e.Field + " " + e.Message }

/*
┌─ aspect ────────────────────────────────────────
│  one aspect type in its portable form
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      typeID       string
├─ out ───────────────────────────────────────────
│      Portable                   fields in position order
│      error                      ErrNotFound
*/

func (s *Service) Export(ctx context.Context, accountID, typeID string) (Portable, error) {
	t, err := s.Get(ctx, accountID, typeID)
	if err != nil {
		return Portable{}, err
	}

	out := Portable{FormatVersion: FormatVersion, Name: t.Name, Fields: make([]PortableField, len(t.Fields))}
	for i, f := range t.Fields {
		out.Fields[i] = PortableField{Label: f.Label, Position: f.Position}
	}

	return out, nil
}

// Validate is the format's whole rulebook, so a caller that is not the API gets the
// same answer. Errors are *FormatError or *textlimit.FieldError; the first one found.
func (p Portable) Validate() error {
	if p.FormatVersion != FormatVersion {
		return &FormatError{Field: "format_version", Unsupported: true,
			Message: "unsupported version " + strconv.Itoa(p.FormatVersion) + "; this server reads version " + strconv.Itoa(FormatVersion)}
	}
	if err := textlimit.Required("name", p.Name, MaxNameLength); err != nil {
		return err
	}
	if len(p.Fields) > MaxImportFields {
		return &FormatError{Field: "fields", Message: "must have at most " + strconv.Itoa(MaxImportFields) + " entries"}
	}
	for i, f := range p.Fields {
		path := "fields[" + strconv.Itoa(i) + "]"
		if err := textlimit.Required(path+".label", f.Label, MaxLabelLength); err != nil {
			return err
		}
		if f.Position < 0 || f.Position > MaxPosition {
			return &FormatError{Field: path + ".position", Message: "must be between 0 and " + strconv.Itoa(MaxPosition)}
		}
	}

	return nil
}

/*
┌─ aspect ────────────────────────────────────────
│  creates a NEW aspect type on a team from a file
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      teamID       string
│      p            Portable
├─ out ───────────────────────────────────────────
│      Type                       fresh ids, fields in order
│      error                      ErrNotFound · *FormatError · *textlimit.FieldError
├─ example ───────────────────────────────────────
│      "Class" on a team that has one  →  "Class (2)"
*/

// Import never touches an existing type: it always inserts, so a file cannot overwrite
// anything. If the team already has a type of that name the new one is called
// "<name> (2)", then "(3)", and so on - aspect_type has no UNIQUE on the name (see
// TestTwoTypesMayShareAName), so this is a courtesy to the person looking at a list of
// identical names, not a constraint. The name that was kept is the one in the response.
//
// Fields are inserted in (position, file order) and numbered 1..n like AddField does,
// so the stored positions are tidy even when the file's were sparse or repeated.
//
// One transaction, and a per-team advisory lock around the name choice, so two imports
// of the same file racing cannot both take "(2)". The lock only serialises imports with
// each other; a CreateType landing in the same instant can still share a name, which is
// legal.
func (s *Service) Import(ctx context.Context, accountID, teamID string, p Portable) (Type, error) {
	if err := p.Validate(); err != nil {
		return Type{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Type{}, fmt.Errorf("aspect: import begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a rollback after commit is a no-op

	var reachable bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM team tm
		      JOIN workspace_member wm
		           ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		     WHERE tm.id = $1::uuid)`, teamID, accountID).Scan(&reachable)
	if err != nil {
		return Type{}, fmt.Errorf("aspect: import scope: %w", err)
	}
	if !reachable {
		return Type{}, ErrNotFound
	}

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('aspect_import:' || $1::text, 0))`, teamID); err != nil {
		return Type{}, fmt.Errorf("aspect: import lock: %w", err)
	}

	taken, err := teamTypeNames(ctx, tx, teamID)
	if err != nil {
		return Type{}, err
	}

	var t Type
	err = tx.QueryRow(ctx,
		`INSERT INTO aspect_type (team_id, name) VALUES ($1::uuid, $2)
		 RETURNING id::text, team_id::text, name, created_at, updated_at`,
		teamID, uniqueName(p.Name, taken),
	).Scan(&t.ID, &t.TeamID, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Type{}, fmt.Errorf("aspect: import type: %w", err)
	}

	ordered := append([]PortableField(nil), p.Fields...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })

	for i, f := range ordered {
		if _, err := tx.Exec(ctx,
			`INSERT INTO aspect_type_field (aspect_type_id, label, position) VALUES ($1::uuid, $2, $3)`,
			t.ID, f.Label, i+1); err != nil {
			return Type{}, fmt.Errorf("aspect: import field %d: %w", i, err)
		}
	}

	t.Fields, err = s.fields(ctx, tx, t.ID)
	if err != nil {
		return Type{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Type{}, fmt.Errorf("aspect: import commit: %w", err)
	}

	return t, nil
}

func teamTypeNames(ctx context.Context, tx pgx.Tx, teamID string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT name FROM aspect_type WHERE team_id = $1::uuid`, teamID)
	if err != nil {
		return nil, fmt.Errorf("aspect: import names: %w", err)
	}
	defer rows.Close()

	taken := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("aspect: import names scan: %w", err)
		}
		taken[n] = true
	}

	return taken, rows.Err()
}

// uniqueName is name if free, else "name (2)", "name (3)"... The base is shortened when
// the suffix would push the result past MaxNameLength, so a clash on a 100-character
// name still imports.
func uniqueName(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}

	for n := 2; ; n++ {
		suffix := " (" + strconv.Itoa(n) + ")"
		base := name
		if over := utf8.RuneCountInString(base) + utf8.RuneCountInString(suffix) - MaxNameLength; over > 0 {
			base = string([]rune(base)[:utf8.RuneCountInString(base)-over])
		}
		if !taken[base+suffix] {
			return base + suffix
		}
	}
}

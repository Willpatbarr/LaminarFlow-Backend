/*
╔═ registry.go ═════════════════════════════════════════════════════════════════════════
║  setting · what settings exist, and what shape each one holds
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Key                  string
║      Scope                string
║      Definition           struct
║      SprintLengthDays     Key
║      BoardColumnSources   Key
║      Registry             map
║      ErrUnknownKey        error
║      ErrWrongScope        error
║      ErrWrongShape        error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/setting  →  Service, which refuses anything not in here
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package setting owns the setting table, and the answer to what a setting key may be.
//
// setting.key is unconstrained text, so `kanban_colums` stores a row successfully, the
// reader asking for `kanban_columns` finds nothing and falls back to its default, and
// nothing errors anywhere. The person who set it watches the product ignore them.
//
// LAM-52 decision 1: the registry is Go, not a table.
//
// A setting_key table with a foreign key is what the database could enforce, and it was
// rejected because it puts the answer in the wrong place. What settings exist is a fact
// about the product, known at compile time, and every consumer of one is Go code that
// has to agree with it - so a registry in the database would be a second copy that a
// deploy could disagree with, in the direction where the code is newer than the rows.
// A CHECK has the same problem plus a migration per setting.
//
// What replaces the database's enforcement is stronger for the failure that actually
// happens: a typo becomes a compile error.
//
//	svc.Set(ctx, me, target, setting.SprintLenghtDays, v)   // does not compile
//	svc.Set(ctx, me, target, "sprint.length_days", v)       // does not compile either
//
// That needs Key to be a struct with an unexported field rather than a named string
// type. A `type Key string` would look like it did the job and would not: Go converts
// an untyped string constant to a named string type implicitly, so passing a literal -
// including a misspelt one - compiles silently. A struct nothing outside this package
// can construct has no such hole.
//
// The one legitimate way to turn a string into a Key is Lookup, which consults the
// registry and is the path the HTTP layer takes, where a key genuinely arrives as text.
package setting

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrUnknownKey means the key is not in Registry. It is the whole point of the package.
var ErrUnknownKey = errors.New("setting: unknown key")

// ErrWrongScope means a real key was written at the wrong level - a team setting stored
// against a workspace, or the reverse.
//
// The second silent failure, and the one the ticket does not name. 0009's arc enforces
// exactly one scope but not which: a team-scoped key written at workspace scope stores
// a row, satisfies every constraint, and is read by nobody.
var ErrWrongScope = errors.New("setting: key does not belong at that scope")

// ErrWrongShape means the value is not what the key holds.
var ErrWrongShape = errors.New("setting: value is not the shape this key holds")

// Scope is where a setting hangs. It mirrors 0009's exclusive arc rather than inventing
// a third level - a project override would be a migration, and no consumer needs one.
type Scope string

const (
	WorkspaceScope Scope = "workspace"
	TeamScope      Scope = "team"
)

// Key is a struct, not a string, and the unexported field is load-bearing - see the
// package comment. Nothing outside this package can build one except through Lookup.
//
// Comparable, so it works as a map key, which is what Registry needs.
type Key struct {
	name string
}

// String is the stored spelling, and what the HTTP layer publishes.
func (k Key) String() string { return k.name }

// The keys that exist. Two, because two consumers are waiting and inventing more would
// be inventing structure nobody asked for - the rule this schema has followed
// throughout.
//
// Vars rather than consts, because a struct cannot be a Go constant. They are never
// reassigned, and nothing outside this package could.
var (
	// SprintLengthDays is 0012's waiting consumer. A new sprint's end_date is
	// computed from it once, at creation, and written down - 0012 is explicit that
	// deriving it on read would let a cadence change retroactively move the end date
	// of every sprint that already ran.
	//
	// Team-scoped, because 0012 says so and gives the reason: a cadence is a team's
	// rhythm, the same argument that makes status team-scoped.
	SprintLengthDays = Key{name: "sprint.length_days"}

	// BoardColumnSources is 0025's waiting consumer, and the name is chosen to say
	// what the thing is rather than what the schema notes call it.
	//
	// It is not the Kanban columns. It is the menu of sources a board may derive its
	// columns from, and a board is one saved arrangement built from that menu - a
	// distinction 0025 spends a paragraph on precisely because "Kanban columns"
	// reads like the board. board.column_sources would have repeated the confusion.
	BoardColumnSources = Key{name: "board.column_sources"}
)

// BoardColumnSource values are the sources a board column may derive from, and they are
// the same closed set board.group_by's CHECK holds.
//
// Exactly one today. 0025 declines to widen the CHECK to 'label' until a
// board_column_label mapping table exists, because a board that cannot render its own
// columns is worse than a board that cannot be grouped by label - and this menu must
// not offer what that CHECK will refuse. registry_test.go asserts the two agree by
// putting each value through the CHECK.
const SourceStatus = "status"

var boardColumnSources = map[string]bool{SourceStatus: true}

/*
┏━ Definition ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one registered key, and what it accepts
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Scope       Scope     the one level it belongs at
┃      Summary     string    for the API document
┃      Validate    func      the value shape, or nil
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      the Registry literal, and nowhere else
*/

// Definition answers LAM-52 decision 2, and the answer is "the shape, not a schema".
//
// A JSON Schema per key is most of the work of the ticket and buys precision nobody
// needs yet. A Go function that says "a positive integer" or "a non-empty list drawn
// from this set" catches the failure the ticket names - a key holding the wrong shape,
// discovered at the reader, far from the writer - and costs four lines per key.
type Definition struct {
	Scope    Scope
	Summary  string
	Validate func(raw json.RawMessage) error
}

// Registry is every setting that exists. Adding one is a line here plus its consumer;
// there is no migration, which is the point of keeping it in Go.
var Registry = map[Key]Definition{
	SprintLengthDays: {
		Scope:   TeamScope,
		Summary: "How many days a new sprint runs. Written into sprint.end_date once, at creation.",
		Validate: func(raw json.RawMessage) error {
			var days int
			if err := json.Unmarshal(raw, &days); err != nil {
				return fmt.Errorf("%w: expected a whole number of days", ErrWrongShape)
			}
			// An upper bound as well as a lower one: a sprint length of
			// 100000 is a typo rather than a cadence, and it would write an
			// end_date centuries out that nobody notices until a report does.
			if days < 1 || days > 365 {
				return fmt.Errorf("%w: expected 1 to 365 days, got %d", ErrWrongShape, days)
			}
			return nil
		},
	},

	BoardColumnSources: {
		Scope:   TeamScope,
		Summary: "The sources a board may derive its columns from. Not a board - the menu a board is built from.",
		Validate: func(raw json.RawMessage) error {
			var sources []string
			if err := json.Unmarshal(raw, &sources); err != nil {
				return fmt.Errorf("%w: expected a list of source names", ErrWrongShape)
			}
			if len(sources) == 0 {
				// An empty menu means no board can have columns, which is
				// not a configuration anyone means.
				return fmt.Errorf("%w: at least one source", ErrWrongShape)
			}
			for _, s := range sources {
				if !boardColumnSources[s] {
					return fmt.Errorf("%w: %q is not a column source", ErrWrongShape, s)
				}
			}
			return nil
		},
	},
}

/*
┌─ setting ───────────────────────────────────────
│  checks a key, its scope and its value together
├─ in ────────────────────────────────────────────
│      key      Key
│      scope    Scope
│      raw      json.RawMessage
├─ out ───────────────────────────────────────────
│      error    ErrUnknownKey · ErrWrongScope · ErrWrongShape
├─ example ───────────────────────────────────────
│      sprint.length_days, team, 14  →  nil
*/

// Check is the whole gate, and every write goes through it. Split out from the Service
// so it is testable without a database and so the API can reject a bad body before
// opening a transaction.
func Check(key Key, scope Scope, raw json.RawMessage) error {
	// A zero Key reaches here from `var k setting.Key`, which is the one Key value
	// that can exist without passing through this package. It is not registered, so
	// it fails like any other unknown.
	def, ok := Registry[key]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKey, key.name)
	}

	if def.Scope != scope {
		return fmt.Errorf("%w: %q is %s-scoped, not %s", ErrWrongScope, key.name, def.Scope, scope)
	}

	if def.Validate == nil {
		return nil
	}

	return def.Validate(raw)
}

/*
┌─ setting ───────────────────────────────────────
│  the one string-to-Key path there is
├─ in ────────────────────────────────────────────
│      name    string    from a request path
├─ out ───────────────────────────────────────────
│      Key
│      bool      false when unregistered
├─ example ───────────────────────────────────────
│      "kanban_colums"  →  false
*/

// Lookup exists because a key genuinely arrives as text over HTTP, and something has to
// turn it into a Key. Routing that through the registry means the conversion and the
// check are the same operation - there is no way to obtain a Key for a name nobody
// registered, so no later code has to remember to validate one.
func Lookup(name string) (Key, bool) {
	k := Key{name: name}
	if _, ok := Registry[k]; !ok {
		return Key{}, false
	}

	return k, true
}

// Keys returns every registered key in a stable order, for the API document and for
// anything that enumerates settings. Sorted, because a map's order would make a
// generated document differ between two runs of the same program.
func Keys() []Key {
	out := make([]Key, 0, len(Registry))
	for k := range Registry {
		out = append(out, k)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

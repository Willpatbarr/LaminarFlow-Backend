package setting_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// The headline. `kanban_colums` used to store a row and be read by nobody; now it
// cannot even be spelled at a call site, and the runtime check catches the one path
// that takes a string.
func TestAnUnregisteredKeyIsRefused(t *testing.T) {
	if _, ok := setting.Lookup("kanban_colums"); ok {
		t.Error("a misspelt key looked up successfully")
	}

	// And the registered spelling does resolve, so this is not passing by
	// refusing everything.
	if _, ok := setting.Lookup("board.column_sources"); !ok {
		t.Error("the real key did not look up")
	}
}

// The compile-time half cannot be asserted by a test that compiles, so this asserts the
// property it rests on: the zero Key is the only Key obtainable without this package's
// cooperation, and it is not registered.
func TestTheZeroKeyIsNotAKey(t *testing.T) {
	var unset setting.Key

	if err := setting.Check(unset, setting.TeamScope, raw(`14`)); err == nil {
		t.Error("a zero Key was accepted")
	}
	if _, ok := setting.Registry[unset]; ok {
		t.Error("the zero Key is in the registry")
	}
}

// A real key at the wrong level stores a row, satisfies every constraint in 0009, and
// is read by nobody - the same silent failure as a typo, one level up.
func TestAKeyAtTheWrongScopeIsRefused(t *testing.T) {
	err := setting.Check(setting.SprintLengthDays, setting.WorkspaceScope, raw(`14`))

	if err == nil {
		t.Fatal("a team-scoped key was accepted at workspace scope")
	}
	if !strings.Contains(err.Error(), "team-scoped") {
		t.Errorf("err = %v, want it to name the scope it belongs at", err)
	}
}

func TestSprintLengthAcceptsOnlyAPlausibleNumberOfDays(t *testing.T) {
	for _, good := range []string{`1`, `14`, `365`} {
		if err := setting.Check(setting.SprintLengthDays, setting.TeamScope, raw(good)); err != nil {
			t.Errorf("%s was refused: %v", good, err)
		}
	}

	// 0 and negatives are incoherent; the upper bound catches a typo that would
	// otherwise write an end_date centuries out.
	for _, bad := range []string{`0`, `-1`, `100000`, `"14"`, `14.5`, `[14]`, `null`} {
		if err := setting.Check(setting.SprintLengthDays, setting.TeamScope, raw(bad)); err == nil {
			t.Errorf("%s was accepted as a sprint length", bad)
		}
	}
}

func TestBoardColumnSourcesAcceptsOnlyRealSources(t *testing.T) {
	if err := setting.Check(setting.BoardColumnSources, setting.TeamScope,
		raw(`["status"]`)); err != nil {
		t.Errorf("the one real source was refused: %v", err)
	}

	for _, bad := range []string{
		`[]`,           // no sources means no board can have columns
		`["label"]`,    // board.group_by's CHECK refuses it - see 0025
		`["statuses"]`, // the typo the CHECK exists to catch, one layer up
		`"status"`,     // not a list
		`[1]`,
	} {
		if err := setting.Check(setting.BoardColumnSources, setting.TeamScope, raw(bad)); err == nil {
			t.Errorf("%s was accepted as a column source list", bad)
		}
	}
}

// Every registered key has to be usable, and the registry is the only place that says
// what a key is - so an entry with no name or no scope would be a key nothing could
// write and nothing could diagnose.
func TestEveryRegisteredKeyIsWellFormed(t *testing.T) {
	if len(setting.Registry) == 0 {
		t.Fatal("the registry is empty, so every test above proves nothing")
	}

	for _, key := range setting.Keys() {
		def := setting.Registry[key]

		if key.String() == "" {
			t.Error("a registered key has no name")
		}
		if def.Scope != setting.WorkspaceScope && def.Scope != setting.TeamScope {
			t.Errorf("%s has scope %q, which is not a scope", key, def.Scope)
		}
		if def.Summary == "" {
			t.Errorf("%s has no summary, so the API document cannot describe it", key)
		}

		// A key reachable by name is a key the HTTP layer can accept. One that
		// is not would be settable from Go and unsettable over the API.
		if _, ok := setting.Lookup(key.String()); !ok {
			t.Errorf("%s is in the registry but does not look up by name", key)
		}
	}
}

// api/openapi.json is committed and CI fails on a diff, so anything generated from the
// registry has to come out the same way twice.
func TestKeysAreListedInAStableOrder(t *testing.T) {
	first := setting.Keys()

	for i := 0; i < 20; i++ {
		again := setting.Keys()
		if len(again) != len(first) {
			t.Fatal("Keys returned a different length")
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatal("Keys returned a different order")
			}
		}
	}

	for i := 1; i < len(first); i++ {
		if first[i-1].String() > first[i].String() {
			t.Errorf("Keys is not sorted: %s before %s", first[i-1], first[i])
		}
	}
}

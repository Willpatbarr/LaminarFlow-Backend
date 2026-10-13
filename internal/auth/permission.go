/*
╔═ permission.go ═══════════════════════════════════════════════════════════════════════
║  auth · what an identity may do
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Permission       struct
║      Permissions      func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  GET /api/v1/auth/me
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package auth

/*
┏━ Permission ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one thing the API can do, as resource:action
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      struct
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      name    string    unexported, so only this file mints one
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      the vars below              closed set, 11 today
*/

// Permission is a closed set, and a struct with an unexported field rather than a
// named string, for the reason docs/api-conventions.md section 6 gives: Go converts an
// untyped string constant to a named string type implicitly, so `type Permission
// string` would still let a typo compile. Here the only values that exist are the vars
// below, and Parse is the one door a string comes in by.
//
// This is a different thing from Scope. A Scope is what a *token* was issued with and
// nothing enforces it yet; a Permission is what the *API* can do, which the frontend
// gates UI on. Roles, when they arrive, are a function from a role to a subset of this
// list - additive, with no change to the wire shape.
type Permission struct {
	name string
}

var (
	TicketRead    = Permission{"ticket:read"}
	TicketWrite   = Permission{"ticket:write"}
	AspectRead    = Permission{"aspect:read"}
	AspectWrite   = Permission{"aspect:write"}
	SettingRead   = Permission{"setting:read"}
	SettingWrite  = Permission{"setting:write"}
	TeamRead      = Permission{"team:read"}
	TeamWrite     = Permission{"team:write"}
	ProjectRead   = Permission{"project:read"}
	ProjectWrite  = Permission{"project:write"}
	AccountDelete = Permission{"account:delete"}
)

// registry is the closed set, in the order the API reports it.
var registry = []Permission{
	TicketRead, TicketWrite,
	AspectRead, AspectWrite,
	SettingRead, SettingWrite,
	TeamRead, TeamWrite,
	ProjectRead, ProjectWrite,
	AccountDelete,
}

// String is the wire name.
func (p Permission) String() string { return p.name }

/*
┌─ auth ──────────────────────────────────────────
│  turns a wire name back into a Permission
├─ in ────────────────────────────────────────────
│      name    string
├─ out ───────────────────────────────────────────
│      Permission
│      bool        false for a name not in the set
├─ example ───────────────────────────────────────
│      "ticket:write"  →  TicketWrite, true
*/

func ParsePermission(name string) (Permission, bool) {
	for _, p := range registry {
		if p.name == name {
			return p, true
		}
	}

	return Permission{}, false
}

/*
┌─ auth ──────────────────────────────────────────
│  every permission this identity holds, as names
├─ in ────────────────────────────────────────────
│      id    Identity      unused until roles exist
├─ out ───────────────────────────────────────────
│      []string            never nil, a fresh slice
├─ example ───────────────────────────────────────
│      session or token  →  all 11
*/

func Permissions(_ Identity) []string {
	// There is no authority model yet: one user in one workspace holds everything,
	// and a token's Scopes are not enforced anywhere, so reporting them as a
	// narrowing here would advertise a limit the API does not apply. The Identity
	// parameter is the seam roles will use; taking it now keeps the callers from
	// changing when they do.
	out := make([]string, len(registry))
	for i, p := range registry {
		out[i] = p.name
	}

	return out
}

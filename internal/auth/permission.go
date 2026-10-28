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
// the API enforces (LAM-91); a Permission is what the *API* can do, which the frontend
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
│      id    Identity
├─ out ───────────────────────────────────────────
│      []string            never nil, a fresh slice
├─ example ───────────────────────────────────────
│      session  →  all 11
│      token    →  its scopes
*/

func Permissions(id Identity) []string {
	// A token holds exactly what it was issued with; a session holds everything,
	// because there is no role model yet.
	if id.TokenID != "" {
		out := make([]string, len(id.Scopes))
		for i, sc := range id.Scopes {
			out[i] = string(sc)
		}

		return out
	}

	out := make([]string, len(registry))
	for i, p := range registry {
		out[i] = p.name
	}

	return out
}

/*
┌─ auth ──────────────────────────────────────────
│  whether this identity may do what p names
├─ in ────────────────────────────────────────────
│      id    Identity
│      p     Permission
├─ out ───────────────────────────────────────────
│      bool
*/

func Allows(id Identity, p Permission) bool {
	if id.TokenID == "" {
		return true
	}

	for _, sc := range id.Scopes {
		if string(sc) == p.name {
			return true
		}
	}

	return false
}

// PermissionNames lists every permission, in the order the API reports it.
func PermissionNames() []string {
	return Permissions(Identity{})
}

/*
╔═ context.go ══════════════════════════════════════════════════════════════════════════
║  auth · identity on the request context
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      NewContext     func
║      FromContext    func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api middleware  →  writes
║      every authenticated handler  →  reads
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package auth

import "context"

// contextKey is unexported so no other package can write an Identity onto a
// context. Only the middleware puts one there, which is what lets a handler
// treat its presence as proof rather than as a hint.
type contextKey struct{}

/*
┌─ auth ──────────────────────────────────────────
│  attaches a resolved identity to a context
├─ in ────────────────────────────────────────────
│      ctx    context.Context
│      id     Identity
├─ out ───────────────────────────────────────────
│      context.Context
*/

func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

/*
┌─ auth ──────────────────────────────────────────
│  reads the identity the middleware resolved
├─ in ────────────────────────────────────────────
│      ctx    context.Context
├─ out ───────────────────────────────────────────
│      Identity           zero when unauthenticated
│      bool               false when unauthenticated
├─ example ───────────────────────────────────────
│      after middleware  →  Identity{…}, true
*/

func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

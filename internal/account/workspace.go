/*
╔═ workspace.go ════════════════════════════════════════════════════════════════════════
║  account · which workspace an account is working in
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      WorkspaceOf    func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      Service.Profile   →  GET /api/v1/auth/me
║      internal/team     →  POST /api/v1/teams/list
║      internal/project  →  POST /api/v1/projects/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package account

/*
┌─ account ───────────────────────────────────────
│  SQL for the workspace an account works in
├─ in ────────────────────────────────────────────
│      accountExpr    string    SQL uuid, never caller text
├─ out ───────────────────────────────────────────
│      string                   scalar subquery, NULL with no membership
├─ example ───────────────────────────────────────
│      "$1::uuid"  →  "(SELECT wm.workspace_id … LIMIT 1)"
*/

// WorkspaceOf is the one definition of "the caller's workspace": the oldest membership,
// ordered by created_at then workspace_id (ADR 0004).
//
// It is a fragment and not a method because the callers that need it are list
// statements in other packages, where the answer has to sit inside the same query as the
// rows it scopes. Resolving it first and passing the id in would be two round trips and
// a window between them; writing the subquery out again would be a second definition
// that could drift from the one `me` reports, which is exactly what the ADR promised
// could not happen.
//
// A caller with no membership gets NULL, and `workspace_id = NULL` is never true - so
// the statement it sits in returns nothing, which is the right answer for an account
// with nowhere to go.
//
// accountExpr is SQL text written by this repository, a placeholder or a column
// reference. It is never a value a request supplied.
func WorkspaceOf(accountExpr string) string {
	return `(SELECT wm.workspace_id FROM workspace_member wm
	          WHERE wm.account_id = ` + accountExpr + `
	          ORDER BY wm.created_at, wm.workspace_id LIMIT 1)`
}

/*
╔═ mcp.go ══════════════════════════════════════════════════════════════════════════════
║  http handler · the MCP endpoint (LAM-197)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      MCPPath             const
║      mcpDeps             struct
║      mcpHandler          func
║      addMCPTool          func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewMux  →  rawRoutes · POST/GET/DELETE /mcp
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/comment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/label"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sprint"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the Model Context Protocol endpoint (Streamable HTTP, stateless). It is not
// under V1: MCP versions itself in its handshake, and the URL is the one a person types into
// `claude mcp add`. Like the change feed it is not an OpenAPI operation, so the OpenAPI
// document does not change; docs/adr/0008-mcp-endpoint.md is its document.
const MCPPath = "/mcp"

// A tool that fails answers an MCP tool error (isError: true) whose text is a JSON object with a
// stable `code` - the codes the REST API raises, plus insufficient_scope for a tool the token's
// scopes do not cover - so an agent branches on the code and never on the prose.

/*
┏━ mcpDeps ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the services the tools call, and the feed their writes publish to
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      tickets, accounts, teams, projects, sprints, labels,
┃      documents, comments, search     *<resource>.Service
┃      feed                            ticketFeed
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewMux                          one per process
*/

// The same services the huma handlers get, and the same ticketFeed, so a write made through
// a tool takes the path a REST write takes: the service writes the row and its activity in
// one transaction, then the tool announces it exactly as the handler does.
type mcpDeps struct {
	tickets   *ticket.Service
	accounts  *account.Service
	teams     *team.Service
	projects  *project.Service
	sprints   *sprint.Service
	labels    *label.Service
	documents *document.Service
	comments  *comment.Service
	search    *search.Service
	feed      ticketFeed
}

/*
┌─ api ───────────────────────────────────────────
│  authenticates, then hands the request to the MCP server
├─ in ────────────────────────────────────────────
│      authSvc    *auth.Service
│      deps       *mcpDeps          nil only from a test of the route table
├─ out ───────────────────────────────────────────
│      http.Handler    401 before any MCP traffic
├─ example ───────────────────────────────────────
│      no credential  →  401 unauthorized
*/

// Authentication is auth.Authenticate, as in the change feed and the huma middleware, and it
// happens before the SDK sees a byte: a request with no valid credential is a 401 in the
// ordinary error envelope, not a JSON-RPC error. The identity rides the request context into
// the tools, which is how a scope is checked per call.
//
// The server is stateless: every POST stands alone, there is no Mcp-Session-Id to lose
// across a restart or a second instance, and GET and DELETE answer 405 (the routes are
// registered anyway so the mux does not hand them to the frontend). Responses are plain JSON
// rather than an event stream, which every client reads and curl can too.
//
// Origin is checked as the feed checks it, because a cookie authenticates here too: a page
// on another site must not be able to drive the caller's tickets with their session.
func mcpHandler(authSvc *auth.Service, deps *mcpDeps) http.Handler {
	server, perms := newMCPServer(deps)

	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	// tools/list shows a caller only the tools its scopes allow, so a read-only token never
	// sees a write tool. tools/call is checked again inside each tool, which is what answers
	// insufficient_scope to a caller that names a tool it was not shown.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}

			list, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, err
			}

			me, authed := auth.FromContext(ctx)
			shown := make([]*mcp.Tool, 0, len(list.Tools))
			for _, t := range list.Tools {
				if authed && auth.Allows(me, perms[t.Name]) {
					shown = append(shown, t)
				}
			}

			filtered := *list
			filtered.Tools = shown

			return &filtered, nil
		}
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeError(w, NewError(http.StatusForbidden, CodeOriginNotAllowed,
				"the Origin of this request is not the host it was sent to"))
			return
		}

		var cookie string
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			cookie = c.Value
		}

		id, err := authSvc.Authenticate(r.Context(), r.Header.Get("Authorization"), cookie)
		if err != nil {
			writeError(w, NewError(http.StatusUnauthorized, defaultCode(http.StatusUnauthorized),
				"this endpoint requires a credential"))
			return
		}

		inner.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), id)))
	})
}

/*
┌─ api ───────────────────────────────────────────
│  registers one tool, with the scope it needs
├─ in ────────────────────────────────────────────
│      s        *mcp.Server
│      perms    map[string]auth.Permission   the tool's name → its scope, filled here
│      name     string
│      desc     string                       written for an agent
│      perm     auth.Permission             the scope the REST endpoint needs
│      write    bool                         false marks it read-only
│      run      func(ctx, me, In) (any, error)
├─ example ───────────────────────────────────────
│      ticket:read token calls a ticket:write tool  →  isError, code insufficient_scope
*/

// The wrapper is the whole of what a tool shares: it refuses a caller that lacks the scope,
// runs the tool, and turns its result into one JSON text block and its error into a tool
// error. The In type is the tool's arguments; the SDK infers the input schema from it and
// rejects arguments that do not fit before run is called.
func addMCPTool[In any](s *mcp.Server, perms map[string]auth.Permission, name, desc string,
	perm auth.Permission, write bool, run func(ctx context.Context, me auth.Identity, in In) (any, error),
) {
	perms[name] = perm

	ann := &mcp.ToolAnnotations{ReadOnlyHint: !write}
	if write {
		no := false
		ann.DestructiveHint = &no
	}

	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc, Annotations: ann},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			me, ok := auth.FromContext(ctx)
			if !ok {
				return toolFailure(NewError(http.StatusUnauthorized, defaultCode(http.StatusUnauthorized),
					"this tool requires a credential")), nil, nil
			}

			if !auth.Allows(me, perm) {
				return toolFailure(NewError(http.StatusForbidden, CodeInsufficientScope,
					"the "+name+" tool needs the "+perm.String()+" scope, which this token does not hold")), nil, nil
			}

			out, err := run(ctx, me, in)
			if err != nil {
				return toolFailure(err), nil, nil
			}

			b, err := json.Marshal(out)
			if err != nil {
				log.Printf("mcp: %s: marshal result: %v", name, err)

				return toolFailure(err), nil, nil
			}

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
		})
}

// toolFailure is an MCP tool error: isError true, one text block holding a JSON object an
// agent can read. An error the API would answer with a status carries its own code and
// detail (and `current`, for a 409, which is the point of one); anything else is a fault on
// this side, logged and reported without its text, as the REST API reports a 500. Located
// error entries keep their message and location but not the value, which the REST envelope
// scrubs for the same reason.
func toolFailure(err error) *mcp.CallToolResult {
	body := map[string]any{"code": defaultCode(http.StatusInternalServerError), "detail": "unexpected error occurred"}

	var se huma.StatusError
	if errors.As(err, &se) {
		if raw, mErr := json.Marshal(se); mErr == nil {
			var full map[string]any
			if json.Unmarshal(raw, &full) == nil {
				body = map[string]any{}
				for _, k := range []string{"status", "code", "detail", "errors", "current", "changed_by", "changed_at"} {
					if v, ok := full[k]; ok {
						body[k] = v
					}
				}
				if _, ok := body["code"]; !ok {
					body["code"] = defaultCode(http.StatusInternalServerError)
				}
				if list, ok := body["errors"].([]any); ok {
					for _, e := range list {
						if m, ok := e.(map[string]any); ok {
							delete(m, "value")
						}
					}
				}
			}
		}
	} else {
		log.Printf("mcp: tool failed: %v", err)
	}

	b, _ := json.Marshal(body)

	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

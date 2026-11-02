package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// LAM-197. The MCP endpoint, driven the way Claude Code drives it: the SDK's own client over
// a real HTTP server, a mcpBearer token in the Authorization header, against a real database.
// Every write below is checked where a person would see it - the activity list over REST and
// the change feed over the WebSocket - because the point of the endpoint is that an agent's
// change is indistinguishable from anyone else's.

// mcpBearer puts the token on every request the client makes, as `--header "Authorization: ..."`
// does for `claude mcp add`.
type mcpBearer struct{ header http.Header }

func (b mcpBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range b.header {
		r.Header[k] = v
	}

	return http.DefaultTransport.RoundTrip(r)
}

// agent connects an MCP client session holding a token with these scopes.
func (f feed) agent(t *testing.T, scopes ...auth.Scope) *mcp.ClientSession {
	t.Helper()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil).Connect(testCtx(t),
		&mcp.StreamableClientTransport{
			Endpoint:   f.srv.URL + MCPPath,
			HTTPClient: &http.Client{Transport: mcpBearer{f.tokenHeader(t, scopes...)}},
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()

	res, err := cs.ListTools(testCtx(t), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	names := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		names[i] = tool.Name
	}
	sort.Strings(names)

	return names
}

// call runs a tool and returns its text, and whether it was a tool error.
func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := cs.CallTool(testCtx(t), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}

	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}

	return text.String(), res.IsError
}

// ok runs a tool that must succeed and decodes its JSON.
func mcpOK(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, into any) {
	t.Helper()

	text, isErr := mcpCall(t, cs, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), into); err != nil {
		t.Fatalf("%s result %q: %v", name, text, err)
	}
}

func errorCode(t *testing.T, text string) string {
	t.Helper()

	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		t.Fatalf("tool error %q is not JSON: %v", text, err)
	}

	return e.Code
}

var (
	readTools  = []string{"get_document", "get_ticket", "list_comments", "list_labels", "list_sprints", "list_tickets", "search"}
	writeTools = []string{"add_comment", "create_ticket", "move_ticket", "save_document", "update_ticket"}
)

func TestMCPRefusesACallerWithoutAValidCredential(t *testing.T) {
	f := newFeed(t)

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`

	for name, header := range map[string]http.Header{
		"no credential":     {},
		"a bogus mcpBearer": {"Authorization": {"Bearer lam_aaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
		"a bogus cookie":    f.cookie("not-a-session"),
		"a non-token":       {"Authorization": {"Basic Zm9vOmJhcg=="}},
		"a malformed lam":   {"Authorization": {"Bearer lam_"}},
	} {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+MCPPath, bytes.NewBufferString(initialize))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range header {
			req.Header[k] = v
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var e Error
		if resp.StatusCode != http.StatusUnauthorized || json.Unmarshal(body, &e) != nil || e.Code != "unauthorized" {
			t.Errorf("%s: status %d body %s, want 401 unauthorized in the error envelope", name, resp.StatusCode, body)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: Content-Type %q, want the error envelope", name, resp.Header.Get("Content-Type"))
		}
	}
}

func TestMCPRevokedAndExpiredTokensAreRefused(t *testing.T) {
	f := newFeed(t)
	svc := auth.NewService(f.pool)

	made, revoked, err := svc.Create(testCtx(t), f.will.AccountID, "gone", []auth.Scope{"ticket:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(testCtx(t), f.will.AccountID, made.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	past := time.Now().Add(-time.Hour)
	expired, err := svc.Issue(testCtx(t), f.will.AccountID, "old", []auth.Scope{"ticket:read"}, &past)
	if err != nil {
		t.Fatal(err)
	}

	for name, tok := range map[string]string{"revoked": revoked, "expired": expired} {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+MCPPath, bytes.NewBufferString(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a %s token got %d, want 401", name, resp.StatusCode)
		}
	}
}

func TestMCPListsOnlyTheToolsATokenMayCall(t *testing.T) {
	f := newFeed(t)

	got := toolNames(t, f.agent(t, "ticket:read"))
	if strings.Join(got, ",") != strings.Join(readTools, ",") {
		t.Errorf("ticket:read sees %v, want exactly %v - no write tool", got, readTools)
	}

	got = toolNames(t, f.agent(t, "ticket:read", "ticket:write"))
	want := append(append([]string{}, readTools...), writeTools...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ticket:read + ticket:write sees %v, want %v", got, want)
	}

	// Team and project reads are their own scopes, as their endpoints are.
	got = toolNames(t, f.agent(t, "team:read", "project:read"))
	if strings.Join(got, ",") != "list_members,list_projects,list_statuses,list_teams" {
		t.Errorf("team:read + project:read sees %v", got)
	}

	if got = toolNames(t, f.agent(t, "setting:read")); len(got) != 0 {
		t.Errorf("a token with no scope any tool needs sees %v, want none", got)
	}
}

func TestMCPAWriteToolCalledWithAReadOnlyTokenIsInsufficientScope(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Untouched", "")
	cs := f.agent(t, "ticket:read")

	for name, args := range map[string]map[string]any{
		"add_comment":   {"ticket_id": tk.ID, "body": "nope"},
		"update_ticket": {"id": tk.ID, "title": "Changed"},
		"move_ticket":   {"id": tk.ID, "status": "Done"},
		"create_ticket": {"project_id": f.project.ID, "title": "Nope"},
		"list_teams":    {},
	} {
		text, isErr := mcpCall(t, cs, name, args)
		if !isErr || errorCode(t, text) != CodeInsufficientScope {
			t.Errorf("%s: isError %v, %s - want a tool error with code %s", name, isErr, text, CodeInsufficientScope)
		}
	}

	// Refused, not half done.
	rec := call(t, f.mux, http.MethodGet, V1+"/tickets/"+tk.ID, nil, f.willC)
	var after ticketWire
	decode(t, rec, &after)
	if after.Title != "Untouched" || after.Version != tk.Version {
		t.Errorf("the ticket changed under a refused call: %+v", after)
	}
	want(t, f.history(t, f.willC, "ticket", tk.ID), "created")
}

func TestMCPReadTools(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Fix the login redirect", "Users land on **/home**.")
	f.ticketWith(t, "Unrelated", "")
	cs := f.agent(t, "ticket:read", "team:read", "project:read")

	var got ticketDetailBody
	mcpOK(t, cs, "get_ticket", map[string]any{"id": tk.ID}, &got)
	if got.Title != "Fix the login redirect" || got.Description != "Users land on **/home**." || got.Documents == nil {
		t.Errorf("get_ticket = %+v", got)
	}

	var list page[ticketBody]
	mcpOK(t, cs, "list_tickets", map[string]any{"project_id": f.project.ID, "text": "login"}, &list)
	if len(list.Items) != 1 || list.Items[0].ID != tk.ID {
		t.Errorf("list_tickets text=login = %+v, want only %s", list.Items, tk.ID)
	}

	mcpOK(t, cs, "list_tickets", map[string]any{"team_id": f.project.TeamID}, &list)
	if len(list.Items) != 2 {
		t.Errorf("list_tickets team_id = %d tickets, want 2", len(list.Items))
	}

	var hits struct {
		Items []searchItem `json:"items"`
	}
	mcpOK(t, cs, "search", map[string]any{"q": "login"}, &hits)
	if len(hits.Items) == 0 || hits.Items[0].ID != tk.ID {
		t.Errorf("search = %+v", hits.Items)
	}

	var teams page[teamBody]
	mcpOK(t, cs, "list_teams", map[string]any{}, &teams)
	if len(teams.Items) != 1 || len(teams.Items[0].Statuses) != 3 {
		t.Errorf("list_teams = %+v", teams)
	}

	if text, isErr := mcpCall(t, cs, "get_ticket", map[string]any{"id": "00000000-0000-4000-8000-000000000000"}); !isErr || errorCode(t, text) != CodeTicketNotFound {
		t.Errorf("get_ticket of nothing: %s (isError %v), want %s", text, isErr, CodeTicketNotFound)
	}
}

// The acceptance walk: find a ticket, move it to In Progress, comment on it, edit its
// description - each seen in the activity list and live on the change feed.
func TestMCPWritesTakeTheRESTWritePath(t *testing.T) {
	f := newFeed(t)
	tk := f.ticketWith(t, "Fix login", "old")
	cs := f.agent(t, "ticket:read", "ticket:write")
	watcher := f.open(t, f.cookie(f.sarahC))

	var moved ticketBody
	mcpOK(t, cs, "move_ticket", map[string]any{"id": tk.ID, "status": "in progress"}, &moved)
	statuses := f.team(t).Statuses
	if moved.StatusID == nil || *moved.StatusID != statuses[1].ID {
		t.Fatalf("move_ticket by name put the ticket in %v, want In Progress %s", moved.StatusID, statuses[1].ID)
	}
	if ev := readEvent(t, watcher); ev.Type != "ticket.updated" || ev.TicketID != tk.ID || ev.ActorID != f.will.AccountID {
		t.Errorf("after move_ticket the feed said %+v", ev)
	}

	var comment commentBody
	mcpOK(t, cs, "add_comment", map[string]any{"ticket_id": tk.ID, "body": "Picking this up."}, &comment)
	if ev := readEvent(t, watcher); ev.Type != "comment.created" || ev.CommentID != comment.ID {
		t.Errorf("after add_comment the feed said %+v", ev)
	}

	var edited ticketBody
	mcpOK(t, cs, "update_ticket", map[string]any{"id": tk.ID, "description": "new **markdown**"}, &edited)
	if edited.Description != "new **markdown**" || edited.Title != "Fix login" || edited.StatusID == nil || *edited.StatusID != statuses[1].ID {
		t.Errorf("update_ticket = %+v, want the description changed and the title and status kept", edited)
	}
	if ev := readEvent(t, watcher); ev.Type != "ticket.updated" || ev.TicketID != tk.ID || ev.Version != edited.Version {
		t.Errorf("after update_ticket the feed said %+v", ev)
	}

	// The activity list a browser shows.
	want(t, f.history(t, f.sarahC, "ticket", tk.ID), "created", "status_changed", "field_changed:description")

	var thread page[commentBody]
	mcpOK(t, cs, "list_comments", map[string]any{"ticket_id": tk.ID}, &thread)
	if len(thread.Items) != 1 || thread.Items[0].Body != "Picking this up." || thread.Items[0].AuthorID == nil || *thread.Items[0].AuthorID != f.will.AccountID {
		t.Errorf("list_comments = %+v", thread.Items)
	}

	// Idempotency is the service's, reached through the tool: a retry creates nothing and
	// announces nothing.
	args := map[string]any{"project_id": f.project.ID, "title": "Once", "idempotency_key": "retry-me"}
	var first, second ticketBody
	mcpOK(t, cs, "create_ticket", args, &first)
	mcpOK(t, cs, "create_ticket", args, &second)
	if first.ID != second.ID {
		t.Errorf("a retried create made %s then %s", first.ID, second.ID)
	}
	if ev := readEvent(t, watcher); ev.Type != "ticket.created" || ev.TicketID != first.ID {
		t.Errorf("after create_ticket the feed said %+v", ev)
	}
	probe := f.ticketWith(t, "Probe", "")
	if ev := readEvent(t, watcher); ev.TicketID != probe.ID {
		t.Errorf("the replay announced %+v before the next real write", ev)
	}
}

func TestMCPAStaleWriteIsAConflictTheAgentCanRead(t *testing.T) {
	f := newFeed(t)
	cs := f.agent(t, "ticket:read", "ticket:write")

	var doc documentBody
	mcpOK(t, cs, "save_document", map[string]any{"title": "Notes", "content": "v1", "project_id": f.project.ID}, &doc)
	if doc.Version != 1 || doc.Body["content"] != "v1" {
		t.Fatalf("created document = %+v", doc)
	}

	var saved documentBody
	mcpOK(t, cs, "save_document", map[string]any{"id": doc.ID, "content": "v2", "if_version": 1}, &saved)
	if saved.Version != 2 || saved.Body["content"] != "v2" || saved.Title != "Notes" {
		t.Errorf("edited document = %+v, want v2 at version 2 with the title kept", saved)
	}

	text, isErr := mcpCall(t, cs, "save_document", map[string]any{"id": doc.ID, "content": "v3", "if_version": 1})
	if !isErr || errorCode(t, text) != CodeDocumentConflict || !strings.Contains(text, `"current"`) {
		t.Errorf("a save at a stale version: %s (isError %v), want document_conflict carrying current", text, isErr)
	}

	var got documentDetailBody
	mcpOK(t, cs, "get_document", map[string]any{"id": doc.ID}, &got)
	if got.Body["content"] != "v2" {
		t.Errorf("the refused save changed the document: %+v", got)
	}
}

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
)

// LAM-129. The change feed filters events by the token's scopes.

// realtimeEventTypes reads the event type constants out of internal/realtime/hub.go, so a
// type added there is seen here without anyone remembering to list it.
func realtimeEventTypes(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "../realtime/hub.go", nil, 0)
	if err != nil {
		t.Fatalf("parse hub.go: %v", err)
	}

	var types []string
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					types = append(types, s)
				}
			}
		}
	}

	return types
}

func TestEveryEventTypeHasAReadPermission(t *testing.T) {
	types := realtimeEventTypes(t)
	if len(types) < 30 {
		t.Fatalf("found only %d event types in hub.go; the parser is reading the wrong thing", len(types))
	}

	seen := map[string]bool{}
	for _, typ := range types {
		seen[typ] = true

		p, ok := eventPermission[typ]
		if !ok {
			t.Errorf("event type %q has no row in eventPermission (internal/api/realtime.go): add the read permission its resource needs", typ)
			continue
		}
		if p != auth.TicketRead && p != auth.TeamRead {
			t.Errorf("event type %q maps to %s; canWatch and docs/realtime.md assume ticket:read or team:read", typ, p)
		}
	}

	for typ := range eventPermission {
		if !seen[typ] {
			t.Errorf("eventPermission has %q, which is not an event type in internal/realtime", typ)
		}
	}
}

func TestFeedFilterPerIdentity(t *testing.T) {
	session := auth.Identity{AccountID: "a"}
	if feedFilter(session) != nil || !canWatch(session) {
		t.Error("a session must receive everything")
	}

	ticketOnly := feedFilter(auth.Identity{TokenID: "t", Scopes: []auth.Scope{"ticket:read"}})
	if !ticketOnly("comment.created") || !ticketOnly("document.updated") || ticketOnly("status.created") {
		t.Error("ticket:read must cover ticket-family events and not status.*")
	}

	teamOnly := feedFilter(auth.Identity{TokenID: "t", Scopes: []auth.Scope{"team:read"}})
	if teamOnly("ticket.created") || !teamOnly("status.deleted") {
		t.Error("team:read must cover status.* only")
	}

	// Write scopes grant no read, and an unknown type is delivered to no token.
	writeOnly := auth.Identity{TokenID: "t", Scopes: []auth.Scope{"ticket:write", "setting:read"}}
	if canWatch(writeOnly) || feedFilter(writeOnly)("ticket.created") {
		t.Error("ticket:write and setting:read must not open the feed")
	}
	if ticketOnly("something.new") {
		t.Error("an unmapped event type must fail closed")
	}
}

func (f feed) tokenHeader(t *testing.T, scopes ...auth.Scope) http.Header {
	t.Helper()

	tok, err := auth.NewService(f.pool).Issue(context.Background(), f.will.AccountID, "scoped", scopes, nil)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	return http.Header{"Authorization": {"Bearer " + tok}}
}

func TestATokenWithNoReadScopeForTheFeedIsRefusedBeforeTheUpgrade(t *testing.T) {
	f := newFeed(t)

	for name, scopes := range map[string][]auth.Scope{
		"setting:read only": {"setting:read"},
		"write scopes only": {"ticket:write", "team:write"},
		"no scopes":         nil,
	} {
		resp := f.refused(t, f.tokenHeader(t, scopes...))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var e Error
		if resp.StatusCode != http.StatusForbidden || json.Unmarshal(body, &e) != nil || e.Code != CodeInsufficientScope {
			t.Errorf("%s: status %d body %s, want 403 %s", name, resp.StatusCode, body, CodeInsufficientScope)
		}
	}
}

func TestATokenOnlyReceivesEventsItsScopesCover(t *testing.T) {
	f := newFeed(t)

	session := f.open(t, f.cookie(f.sarahC))
	ticketTok := f.open(t, f.tokenHeader(t, "ticket:read"))
	teamTok := f.open(t, f.tokenHeader(t, "team:read"))

	f.create(t, f.willC, "scoped")

	// The session proves the event was published; the ticket:read token gets it too.
	if got := readEvent(t, session); got.Type != "ticket.created" {
		t.Fatalf("session event = %+v", got)
	}
	if got := readEvent(t, ticketTok); got.Type != "ticket.created" {
		t.Fatalf("ticket:read token event = %+v", got)
	}

	// The team:read token must hear nothing about a ticket.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, data, err := teamTok.Read(ctx); err == nil {
		t.Errorf("team:read token received %s for a ticket write", data)
	}
}

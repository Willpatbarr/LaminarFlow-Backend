package api

import (
	"net/http"
	"strings"
	"testing"
)

// LAM-121. The member list: who a workspace has, for a reviewer picker and for putting
// names on comments.

type memberWire struct {
	AccountID   string `json:"account_id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	CreatedAt   string `json:"created_at"`
}

func membersOf(t *testing.T, f feed, cookie string, req map[string]any) pageWire[memberWire] {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/members/list", req, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("members/list: %d %.300s", rec.Code, rec.Body)
	}
	var out pageWire[memberWire]
	decode(t, rec, &out)

	return out
}

func TestMembersListsTheCallersWorkspaceWithNamesAndNoSecrets(t *testing.T) {
	f := newFeed(t)

	rec := call(t, f.mux, http.MethodPost, V1+"/members/list", map[string]any{}, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("members/list: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "password") {
		t.Fatalf("the member list leaks a credential column: %s", rec.Body)
	}

	var page pageWire[memberWire]
	decode(t, rec, &page)

	got := map[string]memberWire{}
	for _, m := range page.Items {
		got[m.AccountID] = m
	}
	for _, id := range []string{f.will.AccountID, f.sarah.AccountID} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("member %s missing from %+v", id, page.Items)
		}
		if m.DisplayName == "" || m.Email == "" || m.Role == "" || m.CreatedAt == "" {
			t.Errorf("member %s incomplete: %+v", id, m)
		}
	}

	// Sarah sees the same workspace, so the answer does not depend on who asks.
	if other := membersOf(t, f, f.sarahC, map[string]any{}); len(other.Items) != len(page.Items) {
		t.Errorf("sarah sees %d members, will sees %d", len(other.Items), len(page.Items))
	}
}

func TestMembersExcludesOtherWorkspaces(t *testing.T) {
	f := newFeed(t)

	// Registration joins the one workspace, so move the newcomer to a workspace of their own.
	out, _ := register(t, f.mux, "outsider@example.com")
	outsider := out.AccountID
	if _, err := f.pool.Exec(testCtx(t),
		`WITH w AS (INSERT INTO workspace (name) VALUES ('Elsewhere') RETURNING id)
		 UPDATE workspace_member SET workspace_id = (SELECT id FROM w) WHERE account_id = $1::uuid`,
		outsider); err != nil {
		t.Fatalf("move outsider: %v", err)
	}

	for _, m := range membersOf(t, f, f.willC, map[string]any{}).Items {
		if m.AccountID == outsider {
			t.Fatalf("a member of another workspace is listed: %+v", m)
		}
	}
}

func TestMembersFiltersSortsAndPages(t *testing.T) {
	f := newFeed(t)

	one := membersOf(t, f, f.willC, map[string]any{"limit": 1, "sort": []map[string]any{{"field": "email", "desc": false}}})
	if len(one.Items) != 1 || !one.HasMore || one.NextCursor == nil {
		t.Fatalf("first page = %+v, want one item and a cursor", one)
	}
	next := membersOf(t, f, f.willC, map[string]any{"limit": 1, "cursor": *one.NextCursor, "sort": []map[string]any{{"field": "email", "desc": false}}})
	if len(next.Items) != 1 || next.Items[0].AccountID == one.Items[0].AccountID {
		t.Fatalf("second page repeats or is empty: %+v", next.Items)
	}

	by := membersOf(t, f, f.willC, where(cond("email", "sarah@example.com")))
	if len(by.Items) != 1 || by.Items[0].AccountID != f.sarah.AccountID {
		t.Errorf("email filter = %+v, want sarah", by.Items)
	}

	// password_hash is not in the vocabulary, so it cannot be probed.
	rec := call(t, f.mux, http.MethodPost, V1+"/members/list", where(cond("password_hash", "x")), f.willC)
	assertCode(t, rec, http.StatusUnprocessableEntity, CodeFilterInvalid)
}

func TestMembersNeedsACredentialAndTeamRead(t *testing.T) {
	f := newFeed(t)

	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/members/list", map[string]any{}, ""), http.StatusUnauthorized, "unauthorized")

	tickets := createToken(t, f.mux, f.willC, "ticket:read")
	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/members/list", map[string]any{}, "", bearer(tickets.Token)...),
		http.StatusForbidden, CodeInsufficientScope)

	teams := createToken(t, f.mux, f.willC, "team:read")
	if rec := call(t, f.mux, http.MethodPost, V1+"/members/list", map[string]any{}, "", bearer(teams.Token)...); rec.Code != http.StatusOK {
		t.Errorf("team:read token: %d %s", rec.Code, rec.Body)
	}
}

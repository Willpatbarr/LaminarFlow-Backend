package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type createdToken struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Token     string     `json:"token"`
}

func createToken(t *testing.T, mux *http.ServeMux, cookie string, scopes ...string) createdToken {
	t.Helper()

	if scopes == nil {
		scopes = []string{}
	}
	rec := call(t, mux, http.MethodPost, V1+"/auth/tokens",
		map[string]any{"label": "CI", "scopes": scopes}, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: status %d, body %s", rec.Code, rec.Body)
	}

	var out createdToken
	decode(t, rec, &out)

	return out
}

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

func TestACreatedTokenAuthenticates(t *testing.T) {
	mux, _ := registrationMux(t, false)
	out, cookie := register(t, mux, "will@example.com")

	tok := createToken(t, mux, cookie, "ticket:read")
	if tok.Token == "" || tok.ID == "" || !slices.Equal(tok.Scopes, []string{"ticket:read"}) {
		t.Fatalf("created = %+v", tok)
	}

	rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, "", bearer(tok.Token)...)
	if rec.Code != http.StatusOK {
		t.Fatalf("me with token: status %d, body %s", rec.Code, rec.Body)
	}

	var got struct {
		AccountID   string   `json:"account_id"`
		Via         string   `json:"via"`
		Permissions []string `json:"permissions"`
		Scopes      []string `json:"scopes"`
	}
	decode(t, rec, &got)

	if got.Via != "token" || got.AccountID != out.AccountID {
		t.Errorf("via %q account %q", got.Via, got.AccountID)
	}
	if !slices.Equal(got.Permissions, []string{"ticket:read"}) || !slices.Equal(got.Scopes, []string{"ticket:read"}) {
		t.Errorf("permissions %v scopes %v, want [ticket:read] for both", got.Permissions, got.Scopes)
	}
}

func TestMeReportsNullScopesForASession(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, cookie)
	var raw map[string]any
	decode(t, rec, &raw)

	if v, ok := raw["scopes"]; !ok || v != nil {
		t.Errorf("scopes = %v (present %v), want null", v, ok)
	}
}

func TestARevokedTokenIs401AndLeavesTheList(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	tok := createToken(t, mux, cookie, "ticket:read")

	if rec := call(t, mux, http.MethodDelete, V1+"/auth/tokens/"+tok.ID, nil, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body)
	}

	if rec := call(t, mux, http.MethodGet, V1+"/auth/me", nil, "", bearer(tok.Token)...); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d, want 401", rec.Code)
	}

	var list struct {
		Tokens []createdToken `json:"tokens"`
	}
	decode(t, call(t, mux, http.MethodPost, V1+"/auth/tokens/list", nil, cookie), &list)
	if len(list.Tokens) != 0 {
		t.Errorf("list = %+v, want the revoked token gone", list.Tokens)
	}

	rec := call(t, mux, http.MethodDelete, V1+"/auth/tokens/"+tok.ID, nil, cookie)
	assertCode(t, rec, http.StatusNotFound, CodeTokenNotFound)
}

func TestListShowsLastUsedAndNeverTheSecret(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	tok := createToken(t, mux, cookie, "ticket:read")
	call(t, mux, http.MethodGet, V1+"/auth/me", nil, "", bearer(tok.Token)...)

	rec := call(t, mux, http.MethodPost, V1+"/auth/tokens/list", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); strings.Contains(body, tok.Token) || strings.Contains(body, `"token"`) || strings.Contains(body, "hash") {
		t.Errorf("list leaked credential material: %s", body)
	}

	var list struct {
		Tokens []struct {
			ID         string     `json:"id"`
			LastUsedAt *time.Time `json:"last_used_at"`
		} `json:"tokens"`
	}
	decode(t, rec, &list)
	if len(list.Tokens) != 1 || list.Tokens[0].ID != tok.ID || list.Tokens[0].LastUsedAt == nil {
		t.Errorf("list = %+v, want one token with last_used_at set", list.Tokens)
	}
}

func TestATokenOutsideItsScopeIs403NamingTheScope(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	tok := createToken(t, mux, cookie, "ticket:read")

	rec := call(t, mux, http.MethodPost, V1+"/tickets", map[string]any{}, "", bearer(tok.Token)...)
	assertCode(t, rec, http.StatusForbidden, CodeInsufficientScope)
	if !strings.Contains(rec.Body.String(), "ticket:write") {
		t.Errorf("detail does not name the missing scope: %s", rec.Body)
	}

	// The scope it does hold gets past the middleware: a list is not a 403.
	rec = call(t, mux, http.MethodPost, V1+"/tickets/list", map[string]any{}, "", bearer(tok.Token)...)
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Errorf("ticket:read token on tickets/list: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestATokenCannotCreateListOrRevokeTokens(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	// Even holding every scope there is.
	tok := createToken(t, mux, cookie, "ticket:read", "ticket:write", "account:delete")
	other := createToken(t, mux, cookie, "ticket:read")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"create": call(t, mux, http.MethodPost, V1+"/auth/tokens", map[string]any{"label": "x", "scopes": []string{}}, "", bearer(tok.Token)...),
		"list":   call(t, mux, http.MethodPost, V1+"/auth/tokens/list", nil, "", bearer(tok.Token)...),
		"revoke": call(t, mux, http.MethodDelete, V1+"/auth/tokens/"+other.ID, nil, "", bearer(tok.Token)...),
	} {
		t.Run(name, func(t *testing.T) { assertCode(t, rec, http.StatusForbidden, CodeInsufficientScope) })
	}
}

func TestTokenEndpointsNeedACredential(t *testing.T) {
	mux, _ := registrationMux(t, false)

	if rec := call(t, mux, http.MethodPost, V1+"/auth/tokens", map[string]any{"label": "x", "scopes": []string{}}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("create: status %d, want 401", rec.Code)
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "will@example.com")

	rec := call(t, mux, http.MethodPost, V1+"/auth/tokens",
		map[string]any{"label": "x", "scopes": []string{"ticket:read", "tickets:write"}}, cookie)
	assertCode(t, rec, http.StatusUnprocessableEntity, CodeUnknownScope)
	if !strings.Contains(rec.Body.String(), "body.scopes[1]") {
		t.Errorf("location missing: %s", rec.Body)
	}

	past := time.Now().Add(-time.Hour)
	for name, body := range map[string]map[string]any{
		"blank label": {"label": "   ", "scopes": []string{}},
		"long label":  {"label": string(make([]byte, 101)), "scopes": []string{}},
		"no scopes":   {"label": "x"},
		"past expiry": {"label": "x", "scopes": []string{}, "expires_at": past},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := call(t, mux, http.MethodPost, V1+"/auth/tokens", body, cookie); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status %d, want 422; body %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestAnotherAccountsTokenIsNotFound(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookieA := register(t, mux, "a@example.com")
	_, cookieB := register(t, mux, "b@example.com")

	tok := createToken(t, mux, cookieA)

	assertCode(t, call(t, mux, http.MethodDelete, V1+"/auth/tokens/"+tok.ID, nil, cookieB), http.StatusNotFound, CodeTokenNotFound)
}

func assertCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	var e struct {
		Code string `json:"code"`
	}
	decode(t, rec, &e)

	if rec.Code != status || e.Code != code {
		t.Errorf("got %d %q, want %d %q; body %s", rec.Code, e.Code, status, code, rec.Body)
	}
}

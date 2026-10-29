package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// secret is what every test below submits as a credential. It is long and odd enough that
// finding it anywhere in a response body can only mean it was echoed.
const secret = "Zq9-hunter2-not-a-real-credential"

// TestNoCredentialRouteEchoesASecret (LAM-135) posts bad bodies to every route that accepts
// a credential, or a body that could carry one, and asserts the submitted secret appears
// nowhere in the response - not in errors[].value, not in detail, not anywhere.
//
// Routes covered, from grep -rn -i 'password|token|secret' internal/api and the OpenAPI
// document: POST /auth/register and POST /auth/login take a password; POST /auth/tokens
// is the token-bearing body (it mints one; a caller may put any credential-shaped field
// in it); DELETE /accounts/me takes no body at all, so a body sent to it is the case
// where nothing should read it. There is no password change or reset route.
func TestNoCredentialRouteEchoesASecret(t *testing.T) {
	mux, _ := registrationMux(t, false)
	_, cookie := register(t, mux, "owner@example.com")

	type bad struct {
		name string
		body any
		raw  string
	}

	routes := []struct {
		name   string
		method string
		path   string
		cookie string
		bodies []bad
	}{
		{
			name: "register", method: http.MethodPost, path: V1 + "/auth/register",
			bodies: []bad{
				{name: "extra property", body: map[string]any{"email": "a@example.com", "password": secret, "display_name": "A", "extra": 1}},
				{name: "password wrong type: array", body: map[string]any{"email": "a@example.com", "password": []string{secret}, "display_name": "A"}},
				{name: "password wrong type: object", body: map[string]any{"email": "a@example.com", "password": map[string]any{"password": secret}, "display_name": "A"}},
				{name: "password too short", body: map[string]any{"email": "a@example.com", "password": secret[:5], "display_name": "A"}},
				{name: "password too long", body: map[string]any{"email": "a@example.com", "password": strings.Repeat(secret, 3), "display_name": "A"}},
				{name: "email invalid", body: map[string]any{"email": "not-an-email", "password": secret, "display_name": "A"}},
				{name: "missing field", body: map[string]any{"email": "a@example.com", "password": secret}},
				{name: "misnamed field", body: map[string]any{"email": "a@example.com", "Password": secret, "pass": secret, "display_name": "A"}},
				{name: "malformed JSON", raw: `{"email":"a@example.com","password":"` + secret + `",`},
			},
		},
		{
			name: "login", method: http.MethodPost, path: V1 + "/auth/login",
			bodies: []bad{
				{name: "extra property", body: map[string]any{"email": "a@example.com", "password": secret, "extra": 1}},
				{name: "nested extra property", body: map[string]any{"email": "a@example.com", "password": secret, "extra": map[string]any{"password": secret}}},
				{name: "password wrong type: array", body: map[string]any{"email": "a@example.com", "password": []string{secret}}},
				{name: "password wrong type: object", body: map[string]any{"email": "a@example.com", "password": map[string]any{"new_password": secret}}},
				{name: "email wrong type", body: map[string]any{"email": []string{secret}, "password": secret}},
				{name: "missing field", body: map[string]any{"password": secret}},
				{name: "malformed JSON", raw: `{"email":"a@example.com","password":"` + secret + `"`},
			},
		},
		{
			name: "create token", method: http.MethodPost, path: V1 + "/auth/tokens", cookie: cookie,
			bodies: []bad{
				{name: "extra property", body: map[string]any{"label": "CI", "scopes": []string{}, "token": secret}},
				{name: "extra credential keys", body: map[string]any{"label": "CI", "scopes": []string{}, "password": secret, "api_key": secret, "secret": secret}},
				{name: "label wrong type", body: map[string]any{"label": []string{secret}, "scopes": []string{}}},
				{name: "scopes wrong type", body: map[string]any{"label": "CI", "scopes": secret}},
				{name: "label too long", body: map[string]any{"label": strings.Repeat(secret, 10), "scopes": []string{}}},
				{name: "malformed JSON", raw: `{"label":"CI","token":"` + secret + `"`},
			},
		},
	}

	for _, r := range routes {
		for _, b := range r.bodies {
			t.Run(r.name+"/"+b.name, func(t *testing.T) {
				var rec *httptest.ResponseRecorder
				if b.raw != "" {
					req := httptest.NewRequest(r.method, r.path, strings.NewReader(b.raw))
					req.Header.Set("Content-Type", "application/json")
					if r.cookie != "" {
						req.AddCookie(&http.Cookie{Name: "__Host-laminar_session", Value: r.cookie})
					}
					rec = httptest.NewRecorder()
					mux.ServeHTTP(rec, req)
				} else {
					rec = call(t, mux, r.method, r.path, b.body, r.cookie)
				}

				if rec.Code < 400 || rec.Code >= 500 {
					t.Fatalf("status %d, want a 4xx; body %s", rec.Code, rec.Body)
				}
				assertNoSecret(t, rec.Body.String())
			})
		}
	}

	// Account delete reads no body, so it must succeed without ever looking at one - and
	// say nothing of what it was sent. Last, because it deletes the account.
	t.Run("delete account/body with password is ignored", func(t *testing.T) {
		_, c := register(t, mux, "doomed@example.com")
		rec := call(t, mux, http.MethodDelete, V1+"/accounts/me", map[string]any{"password": secret, "extra": 1}, c)
		assertNoSecret(t, rec.Body.String())
	})
}

func assertNoSecret(t *testing.T, body string) {
	t.Helper()

	for _, form := range []string{secret, strings.ToUpper(secret), secret[:12]} {
		if strings.Contains(body, form) {
			t.Errorf("response body contains the submitted credential %q: %s", form, body)
		}
	}
}

// TestNoOperationEchoesABodyCredential sweeps every operation that takes a JSON body
// and posts it two bad bodies carrying credential-shaped fields. It exists so that a
// route added tomorrow is covered without anyone remembering this ticket: the table above
// names the routes that matter today, this one is every route there is.
//
// No database: huma rejects the bodies before any handler runs, so nil services are
// never reached.
func TestNoOperationEchoesABodyCredential(t *testing.T) {
	mux := http.NewServeMux()
	api := NewHumaAPI(mux, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	mux.HandleFunc("/api/", notFound())

	checked := 0

	for path, item := range api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete,
		} {
			if op == nil || op.RequestBody == nil {
				continue
			}
			if _, ok := op.RequestBody.Content["application/json"]; !ok {
				continue
			}

			concrete := path
			for _, p := range []string{"id", "ticket_id", "document_id", "field_id", "status_id", "column_id", "key"} {
				concrete = strings.ReplaceAll(concrete, "{"+p+"}", "x")
			}

			bodies := []string{
				`{"zz_unexpected":1,"password":"` + secret + `","new_password":"` + secret + `","token":"` + secret + `","nested":{"Secret":["` + secret + `"]}}`,
				`{"password":"` + secret + `"`,
			}
			for _, b := range bodies {
				req := httptest.NewRequest(method, concrete, strings.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)

				if rec.Code < 400 || rec.Code >= 500 {
					t.Errorf("%s %s: status %d, want a 4xx; body %s", method, path, rec.Code, rec.Body)
				}
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("%s %s echoed the credential: %s", method, path, rec.Body)
				}
			}
			checked++
		}
	}

	if checked < 20 {
		t.Fatalf("swept only %d operations; the walk is not finding the API", checked)
	}
}

// TestANestedCredentialKeyIsRedacted covers the values that are still sent - those at a
// query or path location - when one is an arbitrary tree with the credential inside it,
// in an object, in an array, in odd case. The error is handler-built, the path huma's own
// validation does not take, which is why the scrub is a transformer.
func TestANestedCredentialKeyIsRedacted(t *testing.T) {
	mux := http.NewServeMux()
	api := NewHumaAPI(mux, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	huma.Register(api, huma.Operation{
		OperationID: "test-nested", Method: http.MethodGet, Path: "/api/v1/test-nested",
	}, func(context.Context, *struct{}) (*struct{}, error) {
		e := NewError(http.StatusUnprocessableEntity, defaultCode(422), "nope")
		e.Errors = []*huma.ErrorDetail{{
			Message: "bad config", Location: "query.config",
			Value: map[string]any{
				"name":         "ok",
				"auth":         map[string]any{"Password": secret, "list": []any{map[string]any{"API_KEY": secret}, "fine"}},
				"new-password": secret,
			},
		}}

		return nil, e
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/test-nested", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	assertNoSecret(t, rec.Body.String())

	var got struct {
		Errors []struct {
			Value map[string]any `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"name":         "ok",
		"auth":         map[string]any{"Password": redacted, "list": []any{map[string]any{"API_KEY": redacted}, "fine"}},
		"new-password": redacted,
	}
	if len(got.Errors) != 1 || !reflect.DeepEqual(got.Errors[0].Value, want) {
		t.Errorf("value = %#v, want %#v", got.Errors, want)
	}
}

// TestScrubDetailValueRules pins the policy one case at a time, including the case the
// ticket names: a sensitive location whose value is the scalar secret itself.
func TestScrubDetailValueRules(t *testing.T) {
	cases := []struct {
		name string
		in   huma.ErrorDetail
		want any
	}{
		{"scalar at sensitive location", huma.ErrorDetail{Message: "expected length >= 10", Location: "body.password", Value: "short"}, nil},
		{"sensitive location, any case", huma.ErrorDetail{Message: "m", Location: "body.user.NewPassword", Value: "x"}, nil},
		{"sensitive location under an index", huma.ErrorDetail{Message: "m", Location: "body.tokens[2]", Value: "x"}, nil},
		{"header", huma.ErrorDetail{Message: "m", Location: "header.Authorization", Value: "Bearer abc"}, nil},
		{"cookie", huma.ErrorDetail{Message: "m", Location: "cookie.__Host-laminar_session", Value: "abc"}, nil},
		{"raw body that would not parse", huma.ErrorDetail{Message: "invalid character", Location: "body", Value: `{"password":"x"`}, nil},
		{"unexpected property carries the whole object", huma.ErrorDetail{Message: "unexpected property", Location: "body.extra", Value: map[string]any{"a": 1}}, nil},
		{"harmless scalar in the body is dropped too", huma.ErrorDetail{Message: "unknown field", Location: "body.filter.conditions[0].field", Value: "assignee_name"}, nil},
		{"secret pasted into a harmless field", huma.ErrorDetail{Message: "expected length <= 100", Location: "body.label", Value: secret}, nil},
		{"credential in query is dropped by name", huma.ErrorDetail{Message: "m", Location: "query.api_key", Value: "abc"}, nil},
		{"container in query is redacted", huma.ErrorDetail{Message: "m", Location: "query.f", Value: map[string]any{"a": 1, "Token": "x"}}, map[string]any{"a": 1, "Token": redacted}},
		{"query value is kept", huma.ErrorDetail{Message: "m", Location: "query.move_to", Value: "abc"}, "abc"},
		{"no value stays none", huma.ErrorDetail{Message: "m", Location: "body.password"}, nil},
		{"key is not a credential word", huma.ErrorDetail{Message: "m", Location: "path.key", Value: "ui.density"}, "ui.density"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scrubDetailValue(&c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

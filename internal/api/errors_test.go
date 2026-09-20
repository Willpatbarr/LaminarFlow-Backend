package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// envelopeMux builds a mux carrying both kinds of failure: the plain handler
// that answers unmatched /api/ paths, and two huma operations that raise real
// huma errors.
//
// No database. NewHumaAPI takes only a mux, and notFound takes nothing, so the
// envelope is exercisable without Postgres - which is the point of testing it
// here rather than through NewMux.
func envelopeMux(t *testing.T) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()
	api := NewHumaAPI(mux, nil, nil, nil, nil, nil, nil, nil)

	// A failure huma raises on a handler's behalf.
	huma.Register(api, huma.Operation{
		OperationID: "test-raise",
		Method:      http.MethodGet,
		Path:        "/api/v1/test-raise",
		Summary:     "Raises a huma 404",
	}, func(context.Context, *struct{}) (*struct{}, error) {
		return nil, huma.Error404NotFound("ticket not found")
	})

	// A failure huma raises before a handler runs at all: schema validation.
	type body struct {
		Name string `json:"name" required:"true"`
	}
	huma.Register(api, huma.Operation{
		OperationID: "test-validate",
		Method:      http.MethodPost,
		Path:        "/api/v1/test-validate",
		Summary:     "Rejects a body missing a required field",
	}, func(context.Context, *struct{ Body body }) (*struct{}, error) {
		return nil, nil
	})

	mux.HandleFunc("/api/", notFound())

	return mux
}

// contractKeys returns the top-level field names of a JSON object, sorted,
// minus $schema.
//
// $schema is excluded deliberately, and it is the one field the two paths
// genuinely disagree on. huma.DefaultConfig installs a schema-link transformer
// that adds it to every response body huma itself writes; notFound is a plain
// http.HandlerFunc that never passes through that transformer, so its response
// cannot carry it without duplicating huma's URL derivation here - which would
// drift on the next upgrade.
//
// Excluding it is safe because it is tooling metadata, not contract: the error
// contract is the RFC 9457 fields plus code, and a client reads those. The
// alternative - dropping the transformer globally - would change every
// successful response too, which is a larger decision than LAM-53 and belongs
// to whoever owns the OpenAPI document.
func contractKeys(t *testing.T, raw []byte) []string {
	t.Helper()

	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}

	out := make([]string, 0, len(obj))
	for k := range obj {
		if k == "$schema" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	return rec
}

// LAM-53 step 4. The two 404s a client can hit - no such endpoint, and no such
// resource - took different shapes before this: notFound wrote a literal
// {"error":"not found"} as application/json while huma answered problem+json.
// A client could not write one error path that read both.
func TestEveryFailureSharesOneEnvelope(t *testing.T) {
	mux := envelopeMux(t)

	unmatched := do(t, mux, http.MethodGet, "/api/nope", "")
	raised := do(t, mux, http.MethodGet, "/api/v1/test-raise", "")

	if unmatched.Code != http.StatusNotFound || raised.Code != http.StatusNotFound {
		t.Fatalf("status = %d and %d, want 404 from both", unmatched.Code, raised.Code)
	}

	unmatchedType := unmatched.Header().Get("Content-Type")
	raisedType := raised.Header().Get("Content-Type")
	if unmatchedType != raisedType {
		t.Errorf("Content-Type = %q (unmatched) and %q (raised), want the same",
			unmatchedType, raisedType)
	}
	if !strings.Contains(unmatchedType, "application/problem+json") {
		t.Errorf("Content-Type = %q, want problem+json", unmatchedType)
	}

	unmatchedKeys := strings.Join(contractKeys(t, unmatched.Body.Bytes()), ",")
	raisedKeys := strings.Join(contractKeys(t, raised.Body.Bytes()), ",")
	if unmatchedKeys != raisedKeys {
		t.Errorf("fields = %q (unmatched) and %q (raised), want the same",
			unmatchedKeys, raisedKeys)
	}
}

// The field the whole decision was for. A client telling two 404s apart must
// not have to read prose - see the Error doc comment.
func TestEveryFailureCarriesACode(t *testing.T) {
	mux := envelopeMux(t)

	for _, tc := range []struct {
		name, method, path, body, want string
	}{
		{"unmatched path", http.MethodGet, "/api/nope", "", "not_found"},
		{"huma raised", http.MethodGet, "/api/v1/test-raise", "", "not_found"},
		{"validation", http.MethodPost, "/api/v1/test-validate", `{}`, "unprocessable_entity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, mux, tc.method, tc.path, tc.body)

			var got Error
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
			}
			if got.Code != tc.want {
				t.Errorf("code = %q, want %q (body %q)", got.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Code has no omitempty deliberately: a field that is sometimes absent sends a
// client back to parsing detail, which is what it exists to prevent. Asserted
// on the wire rather than on the struct, since omitempty is invisible to a
// round-trip through Error.
func TestCodeIsNeverOmitted(t *testing.T) {
	raw, err := json.Marshal(&Error{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(raw), `"code"`) {
		t.Errorf("marshalled %q, want a code field even when empty", raw)
	}
}

// LAM-58 needs a nested filter failure to name the offending condition by
// position, and huma's Location convention already carries that - which is why
// the envelope adds no per-entry code. Proving Location survives the wrapping
// is what makes that decision safe.
//
// The two rows are not the same case, and the difference is the useful part. A
// value that is present and wrong is reported at the field; a value that is
// absent is reported at the object that should have contained it, because
// that is where JSON Schema evaluates `required`. A filter condition naming an
// unknown field is the first kind, so LAM-58 gets the precise path.
//
// Pinned rather than assumed: if a huma upgrade moved either one, a filter
// error would start pointing at the wrong place and nothing else would fail.
func TestValidationFailureNamesTheFieldByLocation(t *testing.T) {
	mux := envelopeMux(t)

	for _, tc := range []struct {
		name, body, wantLocation string
	}{
		{"value present and wrong", `{"name": 5}`, "body.name"},
		{"value absent", `{}`, "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, mux, http.MethodPost, "/api/v1/test-validate", tc.body)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
			}

			var got Error
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
			}

			if len(got.Errors) == 0 {
				t.Fatalf("errors = empty, want the field named (body %q)", rec.Body.String())
			}
			if got.Errors[0].Location != tc.wantLocation {
				t.Errorf("location = %q, want %q", got.Errors[0].Location, tc.wantLocation)
			}
		})
	}
}

func TestDefaultCodeNamesTheStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusNotFound, "not_found"},
		{http.StatusUnprocessableEntity, "unprocessable_entity"},
		{http.StatusConflict, "conflict"},
		{http.StatusBadRequest, "bad_request"},
		{299, "unknown"},
	} {
		if got := defaultCode(tc.status); got != tc.want {
			t.Errorf("defaultCode(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// The constant and the derived default have to agree, or a handler raising
// CodeNotFound and huma raising its own 404 would disagree about the name of
// the same failure.
func TestNotFoundConstantMatchesTheDerivedDefault(t *testing.T) {
	if CodeNotFound != defaultCode(http.StatusNotFound) {
		t.Errorf("CodeNotFound = %q, derived = %q", CodeNotFound, defaultCode(http.StatusNotFound))
	}
}

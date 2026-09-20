package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"

	"github.com/danielgtaylor/huma/v2"
)

func listOperation(t *testing.T) *huma.Operation {
	t.Helper()

	api := NewHumaAPI(http.NewServeMux(), nil, nil, nil)

	item := api.OpenAPI().Paths[V1+"/tickets/list"]
	if item == nil || item.Post == nil {
		t.Fatal("the ticket list operation is not registered")
	}

	return item.Post
}

// The page-size bound lives in one place and is published in another. Two numbers is
// how a document ends up advertising a limit the server rejects.
func TestThePublishedPageSizeBoundMatchesTheEnforcedOne(t *testing.T) {
	op := listOperation(t)

	schema := op.RequestBody.Content["application/json"].Schema
	if schema.Ref == "" {
		t.Fatal("the list body is inlined, so this test cannot find it")
	}

	name := strings.TrimPrefix(schema.Ref, "#/components/schemas/")
	body := NewHumaAPI(http.NewServeMux(), nil, nil, nil).OpenAPI().Components.Schemas.Map()[name]
	if body == nil {
		t.Fatalf("no schema named %q", name)
	}

	limit := body.Properties["limit"]
	if limit == nil || limit.Maximum == nil {
		t.Fatal("limit has no maximum in the document")
	}

	if int(*limit.Maximum) != filter.MaxLimit {
		t.Errorf("the document caps limit at %v, the server at %d",
			*limit.Maximum, filter.MaxLimit)
	}
}

// A POST that does not write looks like a mistake. The reason it is one is settled in
// §2, and this pins that the reason travels with the operation rather than staying in
// a conversation nobody reading the document was part of.
func TestTheListOperationExplainsWhyAReadIsAPost(t *testing.T) {
	description := listOperation(t).Description

	for _, want := range []string{"POST rather than GET", "Nothing is written"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not say %q", want)
		}
	}
}

// The vocabulary is the part that differs per resource, and a client can only know it
// from the document. Generated from the whitelist, so it cannot describe a field the
// server would reject.
func TestTheListOperationPublishesItsVocabulary(t *testing.T) {
	description := listOperation(t).Description

	for _, want := range []string{"`title`", "`status`", "`label`", "`updated_at`"} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not list %s", want)
		}
	}
	if strings.Contains(description, "`archived_at`") {
		t.Error("the description offers a field the vocabulary does not have")
	}
}

// api/openapi.json is committed and CI fails on a diff, so the document has to be the
// same bytes for the same program. A map's iteration order would make it not be.
func TestTheVocabularyIsListedInAStableOrder(t *testing.T) {
	fields := filter.Schema{
		"zebra": {Expr: "t.z", Kind: filter.Text},
		"alpha": {Expr: "t.a", Kind: filter.Text},
		"mid":   {Expr: "t.m", Kind: filter.Text},
	}

	first := listDescription(fields)
	for i := 0; i < 20; i++ {
		if listDescription(fields) != first {
			t.Fatal("the description differs between two calls on the same schema")
		}
	}

	if strings.Index(first, "`alpha`") > strings.Index(first, "`zebra`") {
		t.Error("the fields are not in sorted order")
	}
}

// Every fault at its own location, in one response. LAM-53's convention, applied to
// the first thing that can fail in more than one place at once.
func TestAFilterFaultBecomesOneErrorsEntryPerProblem(t *testing.T) {
	err := listError(&filter.Error{Faults: []filter.Fault{
		{Location: "body.filter.conditions[0].field", Message: "unknown field", Value: "nope"},
		{Location: "body.sort[0].field", Message: "not sortable", Value: "label"},
	}})

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %T, want *Error", err)
	}
	if e.Status != http.StatusUnprocessableEntity || e.Code != CodeFilterInvalid {
		t.Errorf("status %d code %q, want 422 %q", e.Status, e.Code, CodeFilterInvalid)
	}
	if len(e.Errors) != 2 {
		t.Fatalf("got %d entries, want one per fault", len(e.Errors))
	}
	if e.Errors[0].Location != "body.filter.conditions[0].field" {
		t.Errorf("location = %q", e.Errors[0].Location)
	}
}

// Three failures, three codes, because a client does something different with each.
func TestCursorFailuresCarryTheirOwnCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{filter.ErrCursorMismatch, http.StatusBadRequest, CodeCursorMismatch},
		{filter.ErrBadCursor, http.StatusBadRequest, CodeBadCursor},
	} {
		var e *Error
		if !errors.As(listError(tc.err), &e) {
			t.Fatalf("%v did not map to the envelope", tc.err)
		}
		if e.Status != tc.status || e.Code != tc.code {
			t.Errorf("%v → %d %q, want %d %q", tc.err, e.Status, e.Code, tc.status, tc.code)
		}
	}
}

// Anything else is this server's fault and must not be reported as a client error, or
// a caller stops retrying against a database that is merely down.
func TestAnUnknownListErrorIsNotTurnedIntoAClientError(t *testing.T) {
	boom := errors.New("connection refused")

	if got := listError(boom); !errors.Is(got, boom) {
		t.Errorf("listError swallowed an unrelated error: %v", got)
	}
}

// Marked like every other ticket operation. An unmarked one is public.
func TestTheListOperationRequiresACaller(t *testing.T) {
	if required, _ := listOperation(t).Metadata[RequireAuth].(bool); !required {
		t.Error("the list operation is not marked RequireAuth, so it is public")
	}
}

// And the marking is honoured end to end, not merely present in the metadata.
func TestListWithoutACookieIs401(t *testing.T) {
	mux := testMux(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, V1+"/tickets/list", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

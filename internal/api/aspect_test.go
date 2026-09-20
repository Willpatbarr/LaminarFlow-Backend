package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"

	"github.com/danielgtaylor/huma/v2"
)

// Eight operations, and one of them being public would be a silent hole. Marked
// individually rather than wrapped, so this checks the marking rather than trusting it.
func TestEveryAspectOperationRequiresACaller(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	found := 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, V1+"/aspect-types") {
			continue
		}

		for method, op := range map[string]*huma.Operation{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put, "DELETE": item.Delete,
		} {
			if op == nil {
				continue
			}
			found++

			if required, _ := op.Metadata[RequireAuth].(bool); !required {
				t.Errorf("%s %s is not marked RequireAuth, so it is public", method, path)
			}
		}
	}

	if found != 8 {
		t.Errorf("found %d aspect operations, want 8", found)
	}
}

// A reorder naming the wrong set is 409, not 422: the request is well formed and would
// have been right a moment ago, so the client refetches and retries rather than
// treating its own body as malformed.
func TestAWrongFieldSetIsAConflict(t *testing.T) {
	var e *Error
	if !errors.As(aspectError(aspect.ErrFieldSetWrong), &e) {
		t.Fatal("ErrFieldSetWrong did not map to the envelope")
	}

	if e.Status != http.StatusConflict || e.Code != CodeFieldSetWrong {
		t.Errorf("status %d code %q, want 409 %q", e.Status, e.Code, CodeFieldSetWrong)
	}
}

// One 404 for a missing type and a missing field alike, matching the collapse the
// service makes.
func TestAMissingAspectTypeOrFieldIsOne404(t *testing.T) {
	var e *Error
	if !errors.As(aspectError(aspect.ErrNotFound), &e) {
		t.Fatal("ErrNotFound did not map to the envelope")
	}

	if e.Status != http.StatusNotFound || e.Code != CodeAspectNotFound {
		t.Errorf("status %d code %q, want 404 %q", e.Status, e.Code, CodeAspectNotFound)
	}
}

func TestAnUnknownAspectErrorIsNotTurnedIntoAClientError(t *testing.T) {
	boom := errors.New("connection refused")

	if got := aspectError(boom); !errors.Is(got, boom) {
		t.Errorf("aspectError swallowed an unrelated error: %v", got)
	}
}

// The delete decision has to reach whoever calls the API, not only whoever reads the
// service.
func TestTheRemoveFieldOperationSaysWhatItDoesNotDelete(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	op := doc.Paths[V1+"/aspect-types/{id}/fields/{field_id}"].Delete
	if op == nil {
		t.Fatal("remove-field is not registered")
	}

	for _, want := range []string{"stay in every document", "unrecoverable"} {
		if !strings.Contains(op.Description, want) {
			t.Errorf("the description does not say %q", want)
		}
	}
}

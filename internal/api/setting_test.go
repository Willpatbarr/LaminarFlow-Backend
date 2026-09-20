package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"

	"github.com/danielgtaylor/huma/v2"
)

// LAM-52's whole point, at the HTTP boundary: a typo is now a response rather than a
// stored row and a shrug.
func TestAnUnknownSettingKeyIs422(t *testing.T) {
	_, _, err := resolve(settingKeyInput{Key: "kanban_colums"})

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want the envelope", err)
	}
	if e.Status != http.StatusUnprocessableEntity || e.Code != CodeSettingUnknownKey {
		t.Errorf("status %d code %q, want 422 %q", e.Status, e.Code, CodeSettingUnknownKey)
	}
	if !strings.Contains(e.Detail, "kanban_colums") {
		t.Errorf("detail = %q, want it to name the key that was rejected", e.Detail)
	}
}

func TestARegisteredKeyResolves(t *testing.T) {
	target, key, err := resolve(settingKeyInput{
		Key:                "sprint.length_days",
		settingTargetInput: settingTargetInput{Scope: "team", TargetID: "8b1c"},
	})
	if err != nil {
		t.Fatalf("a registered key was rejected: %v", err)
	}

	if key != setting.SprintLengthDays {
		t.Errorf("key = %v, want SprintLengthDays", key)
	}
	if target.Scope != setting.TeamScope || target.ID != "8b1c" {
		t.Errorf("target = %+v", target)
	}
}

// "Nothing is configured" and "there is no such setting" are different answers, and
// collapsing them is how a typo in a client becomes a default that looks deliberate.
func TestNotSetAndUnknownKeyAreDifferentAnswers(t *testing.T) {
	var notSet, unknown *Error

	if !errors.As(settingError(setting.ErrNotSet), &notSet) ||
		!errors.As(settingError(setting.ErrUnknownKey), &unknown) {
		t.Fatal("one of them did not map to the envelope")
	}

	if notSet.Code == unknown.Code {
		t.Error("an unset setting and an unknown key share a code")
	}
	if notSet.Status != http.StatusNotFound {
		t.Errorf("not-set is %d, want 404", notSet.Status)
	}
	if unknown.Status != http.StatusUnprocessableEntity {
		t.Errorf("unknown-key is %d, want 422 - the server can name what is wrong", unknown.Status)
	}
}

func TestWrongScopeAndWrongShapeCarryTheirOwnCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{setting.ErrWrongScope, CodeSettingWrongScope},
		{setting.ErrWrongShape, CodeSettingInvalidValue},
	} {
		var e *Error
		if !errors.As(settingError(tc.err), &e) {
			t.Fatalf("%v did not map to the envelope", tc.err)
		}
		if e.Code != tc.code || e.Status != http.StatusUnprocessableEntity {
			t.Errorf("%v → %d %q, want 422 %q", tc.err, e.Status, e.Code, tc.code)
		}
	}
}

func TestAnUnknownSettingErrorIsNotTurnedIntoAClientError(t *testing.T) {
	boom := errors.New("connection refused")

	if got := settingError(boom); !errors.Is(got, boom) {
		t.Errorf("settingError swallowed an unrelated error: %v", got)
	}
}

// The registry is Go constants, so the document is the only way a client can learn what
// keys exist. Generated from the registry, so it cannot offer one the server refuses.
func TestTheDocumentPublishesTheRegistry(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	op := doc.Paths[V1+"/settings/{key}"].Put
	if op == nil {
		t.Fatal("set-setting is not registered")
	}

	for _, key := range setting.Keys() {
		if !strings.Contains(op.Description, "`"+key.String()+"`") {
			t.Errorf("the document does not list %s", key)
		}
		if !strings.Contains(op.Description, setting.Registry[key].Summary) {
			t.Errorf("the document does not describe %s", key)
		}
	}
}

// api/openapi.json is committed and CI fails on a diff.
func TestTheRegistryDescriptionIsStable(t *testing.T) {
	first := registryDescription()

	for i := 0; i < 20; i++ {
		if registryDescription() != first {
			t.Fatal("the registry description differs between two calls")
		}
	}
}

func TestEverySettingOperationRequiresACaller(t *testing.T) {
	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	found := 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, V1+"/settings") {
			continue
		}
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Delete} {
			if op == nil {
				continue
			}
			found++
			if required, _ := op.Metadata[RequireAuth].(bool); !required {
				t.Errorf("%s %s is public", op.OperationID, path)
			}
		}
	}

	if found != 4 {
		t.Errorf("found %d setting operations, want 4", found)
	}
}

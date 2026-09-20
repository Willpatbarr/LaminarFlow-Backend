/*
╔═ setting.go ══════════════════════════════════════════════════════════════════════════
║  http handlers · settings, and the registry they are checked against
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      CodeSettingTargetNotFound    const
║      CodeSettingNotSet            const
║      CodeSettingUnknownKey        const
║      CodeSettingWrongScope        const
║      CodeSettingInvalidValue      const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  four operations under /settings
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"

	"github.com/danielgtaylor/huma/v2"
)

// Five codes, because a client does something different with each - and because the
// two that matter are the ones LAM-52 exists to make visible at all. An unknown key
// and a key at the wrong scope both used to be a 200 followed by nothing happening.
const (
	CodeSettingTargetNotFound = "setting_target_not_found"
	CodeSettingNotSet         = "setting_not_set"
	CodeSettingUnknownKey     = "setting_unknown_key"
	CodeSettingWrongScope     = "setting_wrong_scope"
	CodeSettingInvalidValue   = "setting_invalid_value"
)

// A setting is identified by three things - which level, which workspace or team, and
// which key - so scope and target travel as query parameters and the key as a path
// segment. The alternative, /teams/{id}/settings/{key}, would need one route tree per
// scope and there are no team or workspace routes yet to hang them on.
type settingTargetInput struct {
	Scope    string `query:"scope" required:"true" enum:"workspace,team" doc:"Which level the setting hangs at."`
	TargetID string `query:"target_id" required:"true" format:"uuid" doc:"The workspace's or the team's id, matching scope."`
}

type settingKeyInput struct {
	settingTargetInput
	Key string `path:"key" doc:"A registered key. An unregistered one is a 422 rather than a stored row nobody reads."`
}

type setSettingInput struct {
	settingKeyInput
	Body struct {
		Value json.RawMessage `json:"value" required:"true" doc:"The value, whose shape depends on the key."`
	}
}

type settingOutput struct {
	Body struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
}

type settingListOutput struct {
	Body struct {
		Items map[string]json.RawMessage `json:"items" doc:"Registered keys only. A stored row whose key is not registered is skipped."`
	}
}

// settingError maps the service's five failures.
//
// Three of them are 422 rather than 404 on purpose: the key or the value is wrong, and
// the server can name exactly what about it - which is the line docs/api-errors.md
// draws between 400 and 422.
func settingError(err error) error {
	switch {
	case errors.Is(err, setting.ErrNotFound):
		return NewError(http.StatusNotFound, CodeSettingTargetNotFound,
			"no such workspace or team")

	case errors.Is(err, setting.ErrNotSet):
		return NewError(http.StatusNotFound, CodeSettingNotSet,
			"this setting has no value - use the default")

	case errors.Is(err, setting.ErrUnknownKey):
		// The failure LAM-52 exists for. It used to be a stored row and a
		// shrug; it is now the only response a typo can get.
		return NewError(http.StatusUnprocessableEntity, CodeSettingUnknownKey, err.Error())

	case errors.Is(err, setting.ErrWrongScope):
		return NewError(http.StatusUnprocessableEntity, CodeSettingWrongScope, err.Error())

	case errors.Is(err, setting.ErrWrongShape):
		return NewError(http.StatusUnprocessableEntity, CodeSettingInvalidValue, err.Error())
	}

	return err
}

// resolve turns the request's three strings into the two typed things the service
// takes. Lookup is the only string-to-Key path there is, so an unregistered key cannot
// get past this line.
func resolve(in settingKeyInput) (setting.Target, setting.Key, error) {
	key, ok := setting.Lookup(in.Key)
	if !ok {
		return setting.Target{}, setting.Key{}, NewError(http.StatusUnprocessableEntity,
			CodeSettingUnknownKey, "no such setting key: "+in.Key)
	}

	return setting.Target{Scope: setting.Scope(in.Scope), ID: in.TargetID}, key, nil
}

/*
┌─ api ───────────────────────────────────────────
│  registers the four setting operations
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *setting.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  PUT /api/v1/settings/sprint.length_days
*/

func registerSettings(api huma.API, svc *setting.Service) {
	requires := map[string]any{RequireAuth: true}
	registered := registryDescription()

	huma.Register(api, huma.Operation{
		OperationID: "list-settings",
		Method:      http.MethodGet,
		Path:        V1 + "/settings",
		Summary:     "List the settings on one workspace or team",
		Description: "Registered keys only. A stored row whose key is not registered is skipped rather than " +
			"returned or raised - one bad row must not break the settings screen for everyone.\n\n" + registered,
		Metadata: requires,
	}, func(ctx context.Context, in *settingTargetInput) (*settingListOutput, error) {
		me, _ := auth.FromContext(ctx)

		values, err := svc.List(ctx, me.AccountID,
			setting.Target{Scope: setting.Scope(in.Scope), ID: in.TargetID})
		if err != nil {
			return nil, settingError(err)
		}

		out := &settingListOutput{}
		out.Body.Items = make(map[string]json.RawMessage, len(values))
		for key, value := range values {
			out.Body.Items[key.String()] = value
		}

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-setting",
		Method:      http.MethodGet,
		Path:        V1 + "/settings/{key}",
		Summary:     "Read one setting",
		Description: "404 with code setting_not_set means nothing is configured - fall back to the default. " +
			"That is a different answer from an unknown key, which is a 422.\n\n" + registered,
		Metadata: requires,
	}, func(ctx context.Context, in *settingKeyInput) (*settingOutput, error) {
		me, _ := auth.FromContext(ctx)

		target, key, err := resolve(*in)
		if err != nil {
			return nil, err
		}

		value, err := svc.Get(ctx, me.AccountID, target, key)
		if err != nil {
			return nil, settingError(err)
		}

		out := &settingOutput{}
		out.Body.Key, out.Body.Value = key.String(), value

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-setting",
		Method:      http.MethodPut,
		Path:        V1 + "/settings/{key}",
		Summary:     "Write one setting",
		Description: "The key, its scope and the value's shape are all checked before anything is stored. " +
			"A misspelt key, a key at the wrong level, or a value of the wrong shape is a 422 here - " +
			"previously each of those stored a row that nothing ever read.\n\n" + registered,
		Metadata: requires,
	}, func(ctx context.Context, in *setSettingInput) (*settingOutput, error) {
		me, _ := auth.FromContext(ctx)

		target, key, err := resolve(in.settingKeyInput)
		if err != nil {
			return nil, err
		}

		if err := svc.Set(ctx, me.AccountID, target, key, in.Body.Value); err != nil {
			return nil, settingError(err)
		}

		out := &settingOutput{}
		out.Body.Key, out.Body.Value = key.String(), in.Body.Value

		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "unset-setting",
		Method:      http.MethodDelete,
		Path:        V1 + "/settings/{key}",
		Summary:     "Remove one setting, restoring its default",
		Description: "Deletes the row. There is no value that means \"the default\" - absence is the default, " +
			"and writing one would freeze today's default into the row.\n\n" + registered,
		Metadata: requires,
	}, func(ctx context.Context, in *settingKeyInput) (*struct{}, error) {
		me, _ := auth.FromContext(ctx)

		target, key, err := resolve(*in)
		if err != nil {
			return nil, err
		}

		if err := svc.Unset(ctx, me.AccountID, target, key); err != nil {
			return nil, settingError(err)
		}

		return nil, nil
	})
}

// registryDescription publishes the registry in the document, generated from the
// registry itself.
//
// A client cannot discover the valid keys any other way - they are Go constants, not
// rows - so leaving them out of the document would trade one unreachable list for
// another. setting.Keys sorts, because api/openapi.json is committed and CI fails on a
// diff between two runs of the same program.
func registryDescription() string {
	var b strings.Builder
	b.WriteString("Registered keys:\n")

	for _, key := range setting.Keys() {
		def := setting.Registry[key]
		b.WriteString("\n- `" + key.String() + "` (" + string(def.Scope) + "-scoped) — " + def.Summary)
	}

	return b.String()
}

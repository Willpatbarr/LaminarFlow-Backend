/*
╔═ openapi.go ══════════════════════════════════════════════════════════════════════════
║  http handlers · api contract
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewMux         →  every documented route
║      cmd/openapi    →  api/openapi.json
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

/*
┌─ api ───────────────────────────────────────────
│  registers every documented endpoint on mux
├─ in ────────────────────────────────────────────
│      mux          *http.ServeMux
│      authSvc      *auth.Service      nil only for cmd/openapi
│      ticketSvc    *ticket.Service    nil only for cmd/openapi
│      aspectSvc    *aspect.Service    nil only for cmd/openapi
│      settingSvc   *setting.Service   nil only for cmd/openapi
│      teamSvc      *team.Service      nil only for cmd/openapi
│      projSvc      *project.Service   nil only for cmd/openapi
│      accountSvc   *account.Service   nil only for cmd/openapi
├─ out ───────────────────────────────────────────
│      huma.API    the same API cmd/openapi emits
├─ example ───────────────────────────────────────
│      empty mux  →  /api/v1/ping, /docs
*/

func NewHumaAPI(mux *http.ServeMux, authSvc *auth.Service, ticketSvc *ticket.Service, aspectSvc *aspect.Service,
	settingSvc *setting.Service, teamSvc *team.Service, projSvc *project.Service,
	accountSvc *account.Service) huma.API {
	// Before anything can raise an error. huma.NewError is a global, so this
	// is the one place that sets it - see useErrorEnvelope.
	useErrorEnvelope()

	cfg := huma.DefaultConfig("LaminarFlow", SpecVersion)

	// DefaultConfig puts these at the root - /docs, /openapi, /schemas - which
	// left them outside /api/ entirely and unversioned by accident rather than
	// by decision (LAM-54).
	//
	// They move under V1 for two reasons. The document describes one contract:
	// SpecVersion's major tracks V1, and v1 and v2 are meant to run side by
	// side, so a single root document could not be 1.x and 2.x at once. And
	// inside /api/ a mistyped docs URL reaches notFound and gets the JSON
	// envelope, rather than falling through to the frontend catch-all and
	// returning the app shell with status 200.
	//
	// Setting these three is the only thing that works. huma does have a
	// prefix mechanism, driven by the first server URL, but getAPIPrefix is
	// consulted by the docs route and the $schema builder only - never by
	// huma.Register - and setting Servers moved neither in v2.39.1. Verified
	// rather than assumed, because the config field reads as though it would.
	cfg.DocsPath = V1 + "/docs"
	cfg.OpenAPIPath = V1 + "/openapi"
	cfg.SchemasPath = V1 + "/schemas"

	api := humago.New(mux, cfg)

	// authSvc is nil only from cmd/openapi, which registers operations to emit
	// the document and never serves a request, so no handler runs. Anything
	// that does serve requests comes through NewMux, which always has a pool.
	if authSvc != nil {
		api.UseMiddleware(authenticate(api, authSvc))
	}
	registerAuth(api, authSvc)
	registerTickets(api, ticketSvc)
	registerAspectTypes(api, aspectSvc)
	registerSettings(api, settingSvc)
	registerBootstrap(api, teamSvc, projSvc)
	registerAccounts(api, accountSvc)

	registerPing(api)

	return api
}

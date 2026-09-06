/*
╔═ openapi.go ════════════════════════════════════════════════════════════════════════
║  http handlers · api contract
╠═ reached from ══════════════════════════════════════════════════════════════════════
║      NewMux         →  every documented route
║      cmd/openapi    →  api/openapi.json
╚═════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

/*
┌─ api ───────────────────────────────────────────
│  registers every documented endpoint on mux
├─ in ────────────────────────────────────────────
│      mux    *http.ServeMux
├─ out ───────────────────────────────────────────
│      huma.API    the same API cmd/openapi emits
├─ example ───────────────────────────────────────
│      empty mux  →  /api/v1/ping, /docs
*/

func NewHumaAPI(mux *http.ServeMux) huma.API {
	api := humago.New(mux, huma.DefaultConfig("LaminarFlow", "0.1.0"))

	registerPing(api)

	return api
}

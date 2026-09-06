/*
╔═ ping.go ══════════════════════════════════════════════════════════════════════════════
║  http handlers · ping endpoint
╠═ declares ═════════════════════════════════════════════════════════════════════════════
║      PingBody       response schema
║      PingOutput     huma output wrapper
╠═ reached from ═════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  GET /api/v1/ping
╚════════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

/*
┏━ PingBody ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the response contract, mirrored into TypeScript
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Message    string      always "pong"
┃      Time       time.Time   UTC
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      registerPing            one per request
*/

type PingBody struct {
	Message string    `json:"message" doc:"Always \"pong\"." example:"pong"`
	Time    time.Time `json:"time" doc:"Server time, UTC."`
}

/*
┏━ PingOutput ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  wrapper huma reads the JSON body off
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Body       PingBody    the whole response
*/

type PingOutput struct {
	Body PingBody
}

/*
┌─ api ───────────────────────────────────────────
│  declares GET /api/v1/ping and its schema
├─ in ────────────────────────────────────────────
│      api    huma.API
├─ example ───────────────────────────────────────
│      GET /api/v1/ping  →  200 {"message":"pong"}
*/
func registerPing(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "ping",
		Method:      http.MethodGet,
		Path:        "/api/v1/ping",
		Summary:     "Ping the API",
	}, func(ctx context.Context, _ *struct{}) (*PingOutput, error) {
		return &PingOutput{Body: PingBody{Message: "pong", Time: time.Now().UTC()}}, nil
	})
}

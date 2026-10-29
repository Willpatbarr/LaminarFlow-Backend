/*
╔═ export.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · data export
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  registerExport
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"bytes"
	"context"
	"net/http"
	"strconv"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/export"

	"github.com/danielgtaylor/huma/v2"
)

// APIOption supplies a dependency NewHumaAPI takes as an option rather than another
// positional argument: the list is already long, and every test passes nils for it.
type APIOption func(*apiOptions)

type apiOptions struct {
	export *export.Service
}

// WithExport enables the export endpoint's handler. Without it the route is still
// described (cmd/openapi) and answers 500.
func WithExport(svc *export.Service) APIOption {
	return func(o *apiOptions) { o.export = svc }
}

// registerExport registers POST /export. It is a POST although it only reads, because it
// is expensive and not cacheable: the same reasoning as the list endpoints.
func registerExport(api huma.API, svc *export.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "export-data",
		Method:      http.MethodPost,
		Path:        V1 + "/export",
		Summary:     "Export everything as a zip",
		Description: "Session only: a token caller gets 403 `insufficient_scope` (token export waits for an `export:read` permission). " +
			"`application/zip` holding one JSON file per table (`tickets.json`, `documents.json` without bodies, `teams.json`, `projects.json`, `settings.json`, `aspect_types.json`, and so on), " +
			"`documents/<id>.md` with each document's body as Markdown (aspect documents get one section per field, headed by the field's label), and `manifest.json` with the schema version, time and row counts. " +
			"Archived records are included. Passwords, API tokens and sessions are not. Attachment records are in `attachments.json`; the file bytes are not in the zip (use `scripts/backup.sh`). " +
			"All of it is read in one transaction, so the files agree with each other. This is an export for people and scripts, not a backup.",
		Metadata: map[string]any{RequireAuth: true, RequireSession: true},
		Responses: map[string]*huma.Response{
			"200": {Description: "The zip.", Headers: map[string]*huma.Param{
				"Content-Disposition": {Description: "attachment; filename=\"laminarflow-export-<UTC timestamp>.zip\"", Schema: &huma.Schema{Type: huma.TypeString}},
			}, Content: map[string]*huma.MediaType{"application/zip": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}}},
		},
	}, func(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		if svc == nil {
			return nil, NewError(http.StatusInternalServerError, defaultCode(http.StatusInternalServerError), "export is not configured")
		}

		// Built in memory first so a failure is a clean 500, not a truncated 200. A
		// single-user workspace's text is small; attachments are not in it.
		var buf bytes.Buffer
		m, err := svc.Write(ctx, &buf)
		if err != nil {
			return nil, err
		}

		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/zip")
			hctx.SetHeader("Content-Length", strconv.Itoa(buf.Len()))
			hctx.SetHeader("Content-Disposition", `attachment; filename="laminarflow-export-`+m.ExportedAt.Format("20060102T150405Z")+`.zip"`)
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetStatus(http.StatusOK)
			_, _ = hctx.BodyWriter().Write(buf.Bytes())
		}}, nil
	})
}

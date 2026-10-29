package api

import (
	"io/fs"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/activity"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/attachment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/board"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/comment"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/epic"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/export"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/frontend"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/idempotency"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/label"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/milestone"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/project"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/realtime"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/savedview"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/sprint"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/storage"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/team"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MuxOption configures NewMux.
type MuxOption func(*muxConfig)

type muxConfig struct {
	files    storage.Storage
	maxBytes int64
}

// WithAttachments says where uploaded files are kept and how large one may be. Both come
// from config in main; a test passes a temporary folder and a small limit.
func WithAttachments(files storage.Storage, maxBytes int64) MuxOption {
	return func(c *muxConfig) { c.files, c.maxBytes = files, maxBytes }
}

// NewMux wires every route the server answers.
//
// Split out of main so it can be tested: main itself needs a real database URL
// and a listening socket, which is why the routing went unverified until LAM-39.
func NewMux(pool *pgxpool.Pool, bundle fs.FS, hub *realtime.Hub, opts ...MuxOption) *http.ServeMux {
	var mc muxConfig
	for _, o := range opts {
		o(&mc)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", live())
	mux.HandleFunc("GET /healthz/db", ready(pool))
	authSvc, accountSvc, aspectSvc := auth.NewService(pool), account.NewService(pool), aspect.NewService(pool)
	documentSvc := document.NewService(pool)
	NewHumaAPI(mux, authSvc, ticket.NewService(pool), aspectSvc, setting.NewService(pool),
		team.NewService(pool), project.NewService(pool), accountSvc, board.NewService(pool),
		epic.NewService(pool), sprint.NewService(pool), milestone.NewService(pool), label.NewService(pool), documentSvc,
		savedview.NewService(pool), search.NewService(pool), comment.NewService(pool, aspectSvc), activity.NewService(pool), attachmentService(pool, mc), hub,
		WithExport(export.NewService(pool, documentSvc, aspectSvc)),
		WithIdempotency(idempotency.NewStore(pool)))

	// Handlers huma cannot describe. Registered before the /api/ catch-all below for
	// readability only: the mux picks the most specific pattern whatever the order, so
	// GET /api/v1/ws beats /api/ and the frontend's "/" alike.
	for _, rt := range rawRoutes(authSvc, accountSvc, hub) {
		mux.Handle(rt.Method+" "+rt.Path, rt.Handler)
	}

	mux.HandleFunc("/api/", notFound())

	// Everything else is the frontend: real files where they exist, the app
	// shell everywhere else so client-side routing survives a refresh.
	mux.Handle("/", frontend.New(bundle))

	return mux
}

// attachmentService is nil when no storage was configured, which only a test of something
// else does: the upload routes then exist but answer 500, rather than the server starting
// without anywhere to keep files. main always passes one, and refuses to start without.
func attachmentService(pool *pgxpool.Pool, mc muxConfig) *attachment.Service {
	if mc.files == nil {
		return nil
	}

	return attachment.NewService(pool, mc.files, mc.maxBytes)
}

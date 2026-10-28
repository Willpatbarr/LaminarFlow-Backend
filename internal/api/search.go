/*
╔═ search.go ═══════════════════════════════════════════════════════════════════════════
║  http handlers · search
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      SearchOutputBody    struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      NewHumaAPI  →  POST /api/v1/search/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package api

import (
	"context"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/auth"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"

	"github.com/danielgtaylor/huma/v2"
)

type searchItem struct {
	Type       string  `json:"type" enum:"ticket,document,comment,project"`
	ID         string  `json:"id" format:"uuid" doc:"The ticket, document, comment or project the result is."`
	Title      string  `json:"title" doc:"Plain text - escape it when rendering as HTML. A comment shows its first 120 characters."`
	Snippet    string  `json:"snippet" doc:"HTML-safe: the matched text is escaped, then <mark> is put around the matches. Safe to render as HTML as it is."`
	FieldLabel *string `json:"field_label" doc:"The label of the aspect field that matched. Null except for a document result."`
	ParentType *string `json:"parent_type" doc:"ticket or document: what a comment result is on. Null except for a comment result."`
	ParentID   *string `json:"parent_id" format:"uuid" doc:"The ticket or document a comment result is on, so it can be linked. Null except for a comment result."`
	Score      float64 `json:"score" doc:"Higher is better. For ordering only: the number itself means nothing and can change."`
}

// SearchOutputBody is the whole answer: the top results, not a page. There is no
// next_cursor and no has_more, because a person typing into a palette wants the best few
// and refines the query rather than paging.
type SearchOutputBody struct {
	Items []searchItem `json:"items" doc:"Best first. One entry per ticket, document, comment or project, however many of its fields matched."`
}

type searchOutput struct {
	Body SearchOutputBody
}

type searchInput struct {
	Body struct {
		Q     string `json:"q" required:"true" minLength:"1" maxLength:"200" doc:"What to look for: 1 to 200 characters (not bytes), and not only whitespace. Words are stemmed and all must match; \"quoted phrases\", or, and -minus work as in a web search."`
		Type  string `json:"type,omitempty" enum:"ticket,document,comment,project" doc:"Only results of this kind. Omit for all."`
		Limit int    `json:"limit,omitempty" minimum:"1" maximum:"50" doc:"How many results. Omitted means 20. Over the maximum is rejected, not clamped."`
	}
}

/*
┌─ api ───────────────────────────────────────────
│  registers the search operation
├─ in ────────────────────────────────────────────
│      api    huma.API
│      svc    *search.Service    nil only for cmd/openapi
├─ example ───────────────────────────────────────
│      →  POST /api/v1/search/list
*/

func registerSearch(api huma.API, svc *search.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "search",
		Method:      http.MethodPost,
		Path:        V1 + "/search/list",
		Summary:     "Search tickets, projects, documents and comments (a read - POST for the body)",
		Description: "The best matches for `q` across every workspace you belong to, best first. A title match ranks above a match in the text. " +
			"A document is searched by title and by each aspect field's value, and a result names the field that matched in `field_label`. " +
			"Archived tickets and documents are not searched. Returns the top `limit` results: there is no cursor. " +
			"A `q` that is empty or only whitespace is a 422 at `body.q`.",
		Metadata: map[string]any{RequireAuth: true},
	}, func(ctx context.Context, in *searchInput) (*searchOutput, error) {
		me, _ := auth.FromContext(ctx)

		hits, err := svc.Find(ctx, me.AccountID, search.FindParams{
			Q: in.Body.Q, Type: in.Body.Type, Limit: in.Body.Limit,
		})
		if err != nil {
			if e := limitError(err); e != nil {
				return nil, e
			}

			return nil, err
		}

		out := &searchOutput{Body: SearchOutputBody{Items: make([]searchItem, len(hits))}}
		for i, h := range hits {
			out.Body.Items[i] = searchItem{
				Type: h.Type, ID: h.ID, Title: h.Title, Snippet: h.Snippet,
				FieldLabel: h.FieldLabel, ParentType: h.ParentType, ParentID: h.ParentID, Score: h.Score,
			}
		}

		return out, nil
	})
}

/*
╔═ query.go ════════════════════════════════════════════════════════════════════════════
║  search · the read path
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Hit              struct
║      FindParams       struct
║      Types            []string
║      MaxLimit         const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/search/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package search

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"
)

// MaxQueryLength bounds q, in characters (LAM-100).
const MaxQueryLength = 200

// DefaultLimit and MaxLimit bound how many results come back. There is no cursor: this
// is the top N by rank, built for jumping to a known item, not a list to page through.
const (
	DefaultLimit = 20
	MaxLimit     = 50
)

// Types is the closed set of values FindParams.Type takes, and the Type of every Hit.
var Types = []string{"ticket", "document", "comment", "project"}

/*
┏━ Hit ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one search result: a source, not an index row
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Type          string     one of Types
┃      ID            string     the ticket, document, comment or project
┃      Title         string     plain text; a comment shows its first 120 characters
┃      Snippet       string     HTML-safe, with <mark> around matches
┃      FieldLabel    *string    document field rows only
┃      Score         float64    higher is better; a title match outranks any body match
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Service.Find
*/

type Hit struct {
	Type       string
	ID         string
	Title      string
	Snippet    string
	FieldLabel *string
	Score      float64
}

// FindParams is one query. Limit 0 means DefaultLimit.
type FindParams struct {
	Q     string
	Type  string
	Limit int
}

// The markers ts_headline puts around a match. Private-use characters rather than the
// <mark> tags themselves: the snippet has to be HTML-escaped, and escaping after marking
// would escape the tags while escaping before marking would let a word inside an entity
// ("amp" in "&amp;") be highlighted into broken markup. So mark with characters no
// content can carry (they are stripped from the content first), escape, then swap them.
const (
	markOpen  = ""
	markClose = ""
)

// findSQL is one statement, in two steps. hits ranks index rows and keeps the best row
// of each source - a document has one row per field, and ten of them for one document
// is one result, not ten. The outer query then builds snippets for the survivors only,
// because ts_headline is the expensive part and the limit is small.
//
// Both tsvectors are matched with OR so each GIN index serves its half (0037): `search`
// is content, `title_search` is the title. The score adds 1 for a title match, which
// puts it above any body match: ts_rank stays well under 1 for the short texts here.
//
// Scope is the caller's workspaces, through workspace_member, like every other read.
//
// websearch_to_tsquery rather than to_tsquery: it accepts any text a person types
// (quotes, "or", a leading minus) and never raises a syntax error.
const findSQL = `
WITH q AS (SELECT websearch_to_tsquery('english', $2::text) AS tsq),
hits AS (
	SELECT DISTINCT ON (src.kind, src.source_id)
	       src.kind, src.source_id, si.id AS row_id, si.field_id,
	       ts_rank(si.search, q.tsq)::float8
	         + CASE WHEN si.title_search @@ q.tsq THEN 1 ELSE 0 END AS score
	  FROM search_index si
	  CROSS JOIN q
	  JOIN workspace_member wm
	       ON wm.workspace_id = si.workspace_id AND wm.account_id = $1::uuid
	  CROSS JOIN LATERAL (
	       SELECT CASE WHEN si.ticket_id         IS NOT NULL THEN 'ticket'
	                   WHEN si.document_id       IS NOT NULL THEN 'document'
	                   WHEN si.comment_id        IS NOT NULL THEN 'comment'
	                   ELSE 'project' END AS kind,
	              COALESCE(si.ticket_id, si.document_id, si.comment_id, si.project_source_id) AS source_id
	  ) src
	 WHERE (si.search @@ q.tsq OR si.title_search @@ q.tsq)
	   AND ($3::text = '' OR src.kind = $3::text)
	 ORDER BY src.kind, src.source_id, score DESC, si.field_id
),
top AS (
	SELECT * FROM hits ORDER BY score DESC, source_id LIMIT $4::int
)
SELECT t.kind, t.source_id::text,
       CASE WHEN t.kind = 'comment' THEN left(si.title_or_preview, 120) ELSE si.title_or_preview END,
       ts_headline('english', translate(si.content, $5::text, ''), q.tsq,
                   'MaxFragments=1, MinWords=10, MaxWords=30, ShortWord=2, StartSel=' || $6::text || ', StopSel=' || $7::text),
       f.label, t.score
  FROM top t
  CROSS JOIN q
  JOIN search_index si ON si.id = t.row_id
  LEFT JOIN aspect_type_field f ON f.id::text = si.field_id
 ORDER BY t.score DESC, t.source_id`

/*
┌─ search ────────────────────────────────────────
│  the top N ranked results for a query
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            FindParams
├─ out ───────────────────────────────────────────
│      []Hit               best first
│      error               *textlimit.FieldError for q
├─ example ───────────────────────────────────────
│      "login"  →  the ticket "Fix login" above a comment mentioning it
*/

// Find searches every workspace the caller belongs to. Only q is validated here: Type
// and Limit are bounded by the API schema, and a value outside them from another caller
// simply finds nothing or is clamped, which is harmless on a read.
func (s *Service) Find(ctx context.Context, accountID string, p FindParams) ([]Hit, error) {
	q := strings.TrimSpace(p.Q)
	if err := textlimit.Required("q", q, MaxQueryLength); err != nil {
		return nil, err
	}

	limit := p.Limit
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}

	rows, err := s.pool.Query(ctx, findSQL,
		accountID, q, p.Type, limit, markOpen+markClose, markOpen, markClose)
	if err != nil {
		return nil, fmt.Errorf("search: find: %w", err)
	}
	defer rows.Close()

	hits := []Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Type, &h.ID, &h.Title, &h.Snippet, &h.FieldLabel, &h.Score); err != nil {
			return nil, fmt.Errorf("search: find: scan: %w", err)
		}
		h.Snippet = markSnippet(h.Snippet)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: find: %w", err)
	}

	return hits, nil
}

// markSnippet escapes headline, then turns the markers into <mark> tags. Escaping first
// is the order that keeps content from injecting markup; the markers survive it because
// they are not characters html.EscapeString touches.
func markSnippet(headline string) string {
	out := html.EscapeString(headline)
	out = strings.ReplaceAll(out, markOpen, "<mark>")

	return strings.ReplaceAll(out, markClose, "</mark>")
}

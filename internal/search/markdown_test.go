package search_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
)

// LAM-143. PlainText is the one place Markdown becomes the text search indexes.
func TestPlainText(t *testing.T) {
	const id = "0b6f3c1e-6d52-4a4f-9a52-2f7a1d1c9e11"

	for _, tc := range []struct{ name, in, want string }{
		{"image", "see ![pasted](/api/v1/attachments/" + id + ") here", "see [image] here"},
		{"image with empty alt", "![](https://x.test/a.png)", "[image]"},
		{"ticket link", "[QA ticket one](lf://ticket/" + id + ") is open", "QA ticket one is open"},
		{"document link", "[Spec](lf://document/" + id + ")", "Spec"},
		{"project link", "[Core](lf://project/" + id + ")", "Core"},
		{"web link", "[docs](https://example.test/a_b_c?x=(1))", "docs"},
		{"label with brackets", "[fix [WIP] item](lf://ticket/" + id + ") now", "fix [WIP] item now"},
		{"label with escaped bracket", `[a \] b](https://x.test)`, "a ] b"},
		{"link inside emphasis", "**[bold link](https://x.test)**", "bold link"},
		{"image inside link", "[![logo](a.png)](https://x.test)", "[image]"},
		{"bracket without url is text", "keep [this] and [that", "keep [this] and [that"},
		{"nested emphasis", "***both*** and **bold _and italic_** and ~~gone~~", "both and bold and italic and gone"},
		{"star arithmetic is text", "2 * 3 * 4", "2 * 3 * 4"},
		{"snake case is text", "set in_progress_now", "set in_progress_now"},
		{"code span keeps its content", "run `a*b_c [x](y)` now", "run a*b_c [x](y) now"},
		{"double backtick span", "``a ` b``", "a ` b"},
		{"unclosed backtick is text", "a ` b", "a ` b"},
		{"heading", "## Title ##\nbody", "Title\nbody"},
		{"hash without space is text", "#tag", "#tag"},
		{"lists", "- one\n* two\n+ three\n1. four\n2) five\n- [x] done\n  - nested", "one\ntwo\nthree\nfour\nfive\ndone\nnested"},
		{"quote", "> quoted **text**\n>> deeper", "quoted text\ndeeper"},
		{"rule dropped", "a\n---\nb\n***\nc", "a\nb\nc"},
		{"fenced block", "x\n```go\nlet a = **b**\n```\ny", "x\nlet a = **b**\ny"},
		{"tilde fence", "~~~\n# not a heading\n~~~", "# not a heading"},
		{"escapes", `\*literal\* \[x\](y)`, "*literal* [x](y)"},
		{"html passes through", "<img src=x onerror=1> a < b &amp;", "<img src=x onerror=1> a < b &amp;"},
		{"crlf", "a\r\n- b", "a\nb"},
		{"plain stays plain", "Fix the login flow, now.", "Fix the login flow, now."},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := search.PlainText(tc.in); got != tc.want {
				t.Errorf("PlainText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// FieldText runs document field values through PlainText too, nested or not.
func TestFieldTextStripsMarkdown(t *testing.T) {
	got := search.FieldText(map[string]any{"a": "**x** [y](lf://ticket/1)", "b": []any{"![i](u)", float64(3)}})
	if want := "x y [image] 3"; got != want {
		t.Errorf("FieldText = %q, want %q", got, want)
	}
}

// The reported bug end to end: the snippet of a ticket, comment and document whose
// text is Markdown has no syntax in it, and the highlight sits on the plain words.
func TestFindSnippetsArePlainText(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)
	who := member(t, pool)

	const md = "see ![pasted](/api/v1/attachments/abc) and [QA ticket one](lf://ticket/abc) about the **wombat**"
	if _, err := pool.Exec(ctx, `UPDATE ticket SET description = $2 WHERE id = $1::uuid`, f.ticket, md); err != nil {
		t.Fatalf("update ticket: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE comment SET body = $2 WHERE id = $1::uuid`, f.ticketComment, md); err != nil {
		t.Fatalf("update comment: %v", err)
	}

	svc := search.NewService(pool)
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	hits, err := svc.Find(ctx, who, search.FindParams{Q: "wombat"})
	if err != nil || len(hits) < 2 {
		t.Fatalf("find: %v %+v", err, hits)
	}
	for _, h := range hits {
		for _, bad := range []string{"](", "lf://", "/api/v1", "**", "![", "&lt;", "&#"} {
			if strings.Contains(h.Snippet, bad) || strings.Contains(h.Title, bad) {
				t.Errorf("%s hit has %q: snippet %q title %q", h.Type, bad, h.Snippet, h.Title)
			}
		}
		if !strings.Contains(h.Snippet, "[image]") && h.Type == "ticket" {
			t.Errorf("ticket snippet = %q, want [image]", h.Snippet)
		}
		if !strings.Contains(h.Snippet, "<mark>wombat</mark>") {
			t.Errorf("%s snippet = %q, want the match marked", h.Type, h.Snippet)
		}
	}
}

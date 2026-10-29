/*
╔═ stale.go ════════════════════════════════════════════════════════════════════════════
║  comment · approvals that go stale when the reviewed text changes
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      StaleReasonText      const
║      ChangedRunes         func
║      Service.StaleReviews document.Reaction
║      Service.ReviewByID   func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  registered with document.Service.OnWrite; ReviewByID to announce
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package comment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"

	"github.com/jackc/pgx/v5"
)

// StaleReasonText is what comment.stale_reason holds once a document write invalidated an
// approval. The only reason there is; the column is text so another can be added without a
// migration.
const StaleReasonText = "anchored text changed"

// ChangedRunes counts the characters that differ between two texts, in runes: the longer
// length less the common prefix less the common suffix (the suffix is measured on what the
// prefix left, so the two never overlap). A single typo is 1 whatever the length; replacing
// a sentence is about its length. It is a size, not an edit distance: a move counts as the
// whole stretch between the old and new place.
func ChangedRunes(before, after string) int {
	b, a := []rune(before), []rune(after)

	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}

	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix && b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}

	return max(len(b), len(a)) - prefix - suffix
}

// isStale is the whole rule: the changed characters exceed percent of the length of the text
// as it was reviewed. Integer arithmetic, so there is no rounding to argue about. An empty
// before (a caret, or a range past a short field) goes stale as soon as the same range holds
// anything; a caret's own range is empty on both sides and never does.
func isStale(before, after string, percent int) bool {
	return ChangedRunes(before, after)*100 > percent*len([]rune(before))
}

// slice is value[start:end] in runes, clamped to the text: a field that has shrunk below the
// range yields what is left of it, which is itself a change the rule sees.
func slice(text string, start, end int) string {
	r := []rune(text)
	start = min(start, len(r))
	end = min(max(end, start), len(r))

	return string(r[start:end])
}

// threshold is review.stale_threshold_percent for the workspace, or the default when unset or
// unusable (a stored value that no longer validates must not stop documents being saved).
func threshold(ctx context.Context, tx pgx.Tx, workspaceID string) (int, error) {
	raw, err := setting.Workspace(ctx, tx, workspaceID, setting.ReviewStaleThresholdPercent)
	if errors.Is(err, setting.ErrNotSet) {
		return setting.DefaultReviewStaleThresholdPercent, nil
	}
	if err != nil {
		return 0, err
	}

	var pct int
	if json.Unmarshal(raw, &pct) != nil || pct < 0 || pct > 100 {
		return setting.DefaultReviewStaleThresholdPercent, nil
	}

	return pct, nil
}

/*
┌─ comment ───────────────────────────────────────
│  sends approved review requests back to pending when their text changed
├─ in ────────────────────────────────────────────
│      ctx    context.Context
│      tx      pgx.Tx               the document write's transaction
│      c       document.Change
├─ out ───────────────────────────────────────────
│      []string                     ids of the requests sent back to pending
│      error
*/

// StaleReviews is a document.Reaction; register it with document.Service.OnWrite. For each
// approved review request on the document, the anchored text is the field's plain text
// (search.FieldText of body[field_id], or of body["content"] when field_id is null) sliced
// by region_start and region_end, before and after the write. If ChangedRunes of the two
// exceeds the threshold percent of the before length, the request becomes pending with
// stale_reason set, in the write's transaction. The slice is by the stored offsets, which are
// not shifted by edits before the region: a request whose range has drifted compares
// whatever now sits at the old offsets (a known limit, docs/review-requests.md).
//
// Only approved requests can go stale. A pending or changes_requested request has no approval
// to invalidate.
//
// LAM-136: when c.Conversion is set (a normal document became an aspect), every review request
// anchored to ContentKey, whatever its status, is first pointed at the field the text moved to
// (same offsets) or, if the text was not moved as is, at wherever its anchor_text is found in the
// new fields (new offsets); one that cannot be found is left alone. Plain comments have no approval, so they are only re-pointed. A re-anchored approval
// compares the moved text, so the conversion alone does not make it stale. The returned ids
// include the re-anchored requests, which the caller announces as review_request.updated.
func (s *Service) StaleReviews(ctx context.Context, tx pgx.Tx, c document.Change) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, field_id::text, region_start, region_end, COALESCE(review_status, ''), COALESCE(anchor_text, '')
		   FROM comment
		  WHERE document_id = $1::uuid
		    AND (($2 AND region_start IS NOT NULL) OR review_status = 'approved')
		  ORDER BY created_at, id FOR UPDATE`, c.DocumentID, c.Conversion != nil)
	if err != nil {
		return nil, fmt.Errorf("comment: reviews: %w", err)
	}

	type anchor struct {
		id          string
		fieldID     *string
		start, end  int
		hasRegion   bool
		approved    bool
		anchorText  string
		before, aft string // body keys the text was read from before and after the write
	}
	var reviews []anchor
	for rows.Next() {
		var (
			a      anchor
			start  *int
			end    *int
			status string
		)
		if err := rows.Scan(&a.id, &a.fieldID, &start, &end, &status, &a.anchorText); err != nil {
			rows.Close()
			return nil, fmt.Errorf("comment: reviews scan: %w", err)
		}
		a.before = document.ContentKey
		if a.fieldID != nil {
			a.before = *a.fieldID
		}
		a.aft = a.before
		if start != nil && end != nil {
			a.start, a.end, a.hasRegion = *start, *end, true
		}
		a.approved = status == "approved"
		reviews = append(reviews, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comment: reviews rows: %w", err)
	}

	var changed []string

	// LAM-136: the document became an aspect, so what anchored into ContentKey follows the text
	// into its field, in this transaction. Open and decided requests alike; the status is not
	// touched. Plain comments (review_status null) are re-pointed the same way.
	if c.Conversion != nil {
		for i := range reviews {
			a := &reviews[i]
			if a.fieldID != nil || !a.hasRegion {
				continue
			}
			field, start, end, ok := reanchor(c, a.start, a.end, a.anchorText)
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE comment SET field_id = $2::uuid, region_start = $3, region_end = $4, updated_at = now()
				  WHERE id = $1::uuid`, a.id, field, start, end); err != nil {
				return nil, fmt.Errorf("comment: re-anchor review: %w", err)
			}
			a.aft, a.start, a.end = field, start, end
			changed = append(changed, a.id)
		}
	}

	var approved []anchor
	for _, a := range reviews {
		if a.approved && a.hasRegion {
			approved = append(approved, a)
		}
	}
	if len(approved) == 0 {
		return changed, nil
	}

	percent, err := threshold(ctx, tx, c.WorkspaceID)
	if err != nil {
		return nil, err
	}

	for _, a := range approved {
		if a.aft != a.before && c.Conversion != nil && c.Conversion.MovedTo == "" {
			// Re-anchored by search: the same text by construction, so nothing went stale.
			continue
		}
		before := slice(search.FieldText(c.Before[a.before]), a.start, a.end)
		after := slice(search.FieldText(c.After[a.aft]), a.start, a.end)
		if !isStale(before, after, percent) {
			continue
		}

		if _, err := tx.Exec(ctx,
			`UPDATE comment SET review_status = 'pending', stale_reason = $2, updated_at = now()
			  WHERE id = $1::uuid`, a.id, StaleReasonText); err != nil {
			return nil, fmt.Errorf("comment: stale review: %w", err)
		}
		changed = append(changed, a.id)
	}

	return changed, nil
}

// reanchor says where a request that anchored into ContentKey points once the document is an
// aspect. When the text moved to a field it is that field at the same offsets (the text moved
// unchanged); otherwise the stored anchor_text is searched for in the fields in type order and
// the offsets are the first match, in code points. ok is false when there is nothing to point
// at: the text was not carried, the anchor_text is empty (a caret) or is nowhere in the body.
func reanchor(c document.Change, start, end int, anchorText string) (field string, newStart, newEnd int, ok bool) {
	if c.Conversion.MovedTo != "" {
		return c.Conversion.MovedTo, start, end, true
	}
	if anchorText == "" {
		return "", 0, 0, false
	}

	needle := []rune(anchorText)
	for _, id := range c.Conversion.Fields {
		hay := []rune(search.FieldText(c.After[id]))
		for i := 0; i+len(needle) <= len(hay); i++ {
			if string(hay[i:i+len(needle)]) == anchorText {
				return id, i, i + len(needle), true
			}
		}
	}

	return "", 0, 0, false
}

// ReviewByID reads one review request as it is now, for announcing a change made elsewhere
// (the document write above). Not scoped by reach: the caller is server code that has just
// written it. ErrNotFound when it is gone or not a review request.
func (s *Service) ReviewByID(ctx context.Context, id string) (ReviewRequest, error) {
	return s.reviewByID(ctx, s.pool, id)
}

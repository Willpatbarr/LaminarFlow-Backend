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
// Only approved requests are touched. A pending or changes_requested request has no approval
// to invalidate.
func (s *Service) StaleReviews(ctx context.Context, tx pgx.Tx, c document.Change) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text, field_id::text, region_start, region_end FROM comment
		  WHERE document_id = $1::uuid AND review_status = 'approved'
		  ORDER BY created_at, id FOR UPDATE`, c.DocumentID)
	if err != nil {
		return nil, fmt.Errorf("comment: approved reviews: %w", err)
	}

	type anchor struct {
		id         string
		key        string
		start, end int
	}
	var approved []anchor
	for rows.Next() {
		var (
			a       anchor
			fieldID *string
			start   *int
			end     *int
		)
		if err := rows.Scan(&a.id, &fieldID, &start, &end); err != nil {
			rows.Close()
			return nil, fmt.Errorf("comment: approved reviews scan: %w", err)
		}
		a.key = document.ContentKey
		if fieldID != nil {
			a.key = *fieldID
		}
		if start == nil || end == nil {
			continue
		}
		a.start, a.end = *start, *end
		approved = append(approved, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("comment: approved reviews rows: %w", err)
	}
	if len(approved) == 0 {
		return nil, nil
	}

	percent, err := threshold(ctx, tx, c.WorkspaceID)
	if err != nil {
		return nil, err
	}

	var staled []string
	for _, a := range approved {
		before := slice(search.FieldText(c.Before[a.key]), a.start, a.end)
		after := slice(search.FieldText(c.After[a.key]), a.start, a.end)
		if !isStale(before, after, percent) {
			continue
		}

		if _, err := tx.Exec(ctx,
			`UPDATE comment SET review_status = 'pending', stale_reason = $2, updated_at = now()
			  WHERE id = $1::uuid`, a.id, StaleReasonText); err != nil {
			return nil, fmt.Errorf("comment: stale review: %w", err)
		}
		staled = append(staled, a.id)
	}

	return staled, nil
}

// ReviewByID reads one review request as it is now, for announcing a change made elsewhere
// (the document write above). Not scoped by reach: the caller is server code that has just
// written it. ErrNotFound when it is gone or not a review request.
func (s *Service) ReviewByID(ctx context.Context, id string) (ReviewRequest, error) {
	return s.reviewByID(ctx, s.pool, id)
}

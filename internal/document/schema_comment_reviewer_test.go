package document

import (
	"context"
	"testing"
)

// The constraints 0018_comment_reviewer.sql claims.
//
// Here rather than in internal/migrate for the same reason as comment's own
// tests: building a review request to attach a reviewer to means inserting a
// document, and TestNoSQLOutsideThisPackage bars every other package from
// issuing SQL against document.
//
// The bulk of this file is about one invariant - a reviewer can only be
// attached to a comment that is acting as a review request - which LAM-25
// step 3 assigns to application code and which the schema enforces instead.
func TestCommentReviewerConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	newDoc := func(t *testing.T, title string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO document (workspace_id, title) VALUES ($1::uuid, $2)
             RETURNING id::text`, ws, title,
		).Scan(&id); err != nil {
			t.Fatalf("create document: %v", err)
		}

		return id
	}
	// newComment writes a comment on its own document. status "" is a plain
	// comment; anything else makes it a review request.
	newComment := func(t *testing.T, label, status string) string {
		t.Helper()

		doc := newDoc(t, label)

		var reviewStatus *string
		if status != "" {
			reviewStatus = &status
		}

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO comment (document_id, body, review_status)
             VALUES ($1::uuid, 'please review', $2) RETURNING id::text`, doc, reviewStatus,
		).Scan(&id); err != nil {
			t.Fatalf("create comment: %v", err)
		}

		return id
	}
	newReviewer := func(t *testing.T, email string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES ($1, 'hash', $1) RETURNING id::text`, email,
		).Scan(&id); err != nil {
			t.Fatalf("create account: %v", err)
		}

		return id
	}
	assign := func(commentID, accountID string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO comment_reviewer (comment_id, account_id)
             VALUES ($1::uuid, $2::uuid)`, commentID, accountID)
		return err
	}
	countReviewers := func(t *testing.T, commentID string) int {
		t.Helper()

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM comment_reviewer WHERE comment_id = $1::uuid`, commentID,
		).Scan(&n); err != nil {
			t.Fatalf("count reviewers: %v", err)
		}

		return n
	}

	t.Run("accepts a reviewer on a review request", func(t *testing.T) {
		request := newComment(t, "happy-path", "pending")

		if err := assign(request, newReviewer(t, "cr-happy@example.com")); err != nil {
			t.Fatalf("assign a reviewer: %v", err)
		}
	})

	// LAM-25 step 3, enforced by the schema rather than left to application
	// code that does not exist.
	t.Run("rejects a reviewer on a plain comment", func(t *testing.T) {
		plain := newComment(t, "plain", "")

		err := assign(plain, newReviewer(t, "cr-plain@example.com"))
		wantPgError(t, err, foreignKeyViolation, "a reviewer on a comment that is not a review request")
	})

	// The CHECK, doing the job the foreign key cannot. Writing false
	// explicitly would otherwise match a plain comment's (id, false) and pass
	// the foreign key cleanly.
	t.Run("rejects a caller writing is_review_request false", func(t *testing.T) {
		plain := newComment(t, "explicit-false", "")

		_, err := pool.Exec(ctx,
			`INSERT INTO comment_reviewer (comment_id, account_id, is_review_request)
             VALUES ($1::uuid, $2::uuid, false)`,
			plain, newReviewer(t, "cr-false@example.com"))
		wantPgError(t, err, checkViolation, "a reviewer row claiming its parent is not a review request")
	})

	t.Run("rejects a reviewer on a comment that does not exist", func(t *testing.T) {
		err := assign("00000000-0000-0000-0000-000000000000", newReviewer(t, "cr-ghost@example.com"))
		wantPgError(t, err, foreignKeyViolation, "a reviewer on a comment that does not exist")
	})

	t.Run("rejects an account that does not exist", func(t *testing.T) {
		request := newComment(t, "ghost-account", "pending")

		err := assign(request, "00000000-0000-0000-0000-000000000000")
		wantPgError(t, err, foreignKeyViolation, "an account that does not exist")
	})

	t.Run("rejects the same reviewer twice on one request", func(t *testing.T) {
		request := newComment(t, "duplicate", "pending")
		reviewer := newReviewer(t, "cr-dup@example.com")

		if err := assign(request, reviewer); err != nil {
			t.Fatalf("first assignment: %v", err)
		}

		err := assign(request, reviewer)
		wantPgError(t, err, uniqueViolation, "the same reviewer assigned twice")
	})

	t.Run("accepts several reviewers on one request", func(t *testing.T) {
		request := newComment(t, "several", "pending")

		for _, email := range []string{"cr-a@example.com", "cr-b@example.com", "cr-c@example.com"} {
			if err := assign(request, newReviewer(t, email)); err != nil {
				t.Fatalf("assign %s: %v", email, err)
			}
		}
		if got := countReviewers(t, request); got != 3 {
			t.Errorf("request has %d reviewers, want 3", got)
		}
	})

	// The transition the whole design exists to permit. A composite foreign
	// key on review_status itself would block this, or would have to cascade
	// on every review that ever completes.
	t.Run("a review request can be approved with reviewers attached", func(t *testing.T) {
		request := newComment(t, "approve", "pending")

		if err := assign(request, newReviewer(t, "cr-approve@example.com")); err != nil {
			t.Fatalf("assign: %v", err)
		}

		for _, status := range []string{"changes_requested", "approved"} {
			if _, err := pool.Exec(ctx,
				`UPDATE comment SET review_status = $1 WHERE id = $2::uuid`, status, request,
			); err != nil {
				t.Fatalf("move the request to %s: %v", status, err)
			}
			if got := countReviewers(t, request); got != 1 {
				t.Errorf("after moving to %s the request has %d reviewers, want 1", status, got)
			}
		}
	})

	// The one transition it blocks, asserted so it is a decision rather than
	// something found in production.
	t.Run("a review request with reviewers cannot become a plain comment", func(t *testing.T) {
		request := newComment(t, "demote", "pending")

		if err := assign(request, newReviewer(t, "cr-demote@example.com")); err != nil {
			t.Fatalf("assign: %v", err)
		}

		// Refused by the foreign key, not the CHECK: the parent's generated
		// key moves from (id, true) to (id, false) and the reviewer row is
		// still pointing at the old one. NO ACTION, so nothing follows it.
		_, err := pool.Exec(ctx,
			`UPDATE comment SET review_status = NULL WHERE id = $1::uuid`, request)
		wantPgError(t, err, foreignKeyViolation, "demoting a review request that still has reviewers")
	})

	// ...and that the block lifts once the reviewers are gone, so it is a
	// sequencing rule rather than a one-way door.
	t.Run("demotion works once the reviewers are unassigned", func(t *testing.T) {
		request := newComment(t, "demote-clean", "pending")
		reviewer := newReviewer(t, "cr-demote-clean@example.com")

		if err := assign(request, reviewer); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM comment_reviewer WHERE comment_id = $1::uuid`, request,
		); err != nil {
			t.Fatalf("unassign: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE comment SET review_status = NULL WHERE id = $1::uuid`, request,
		); err != nil {
			t.Fatalf("demote after unassigning: %v", err)
		}
	})

	t.Run("deleting the comment deletes its reviewers", func(t *testing.T) {
		request := newComment(t, "comment-cascade", "pending")

		if err := assign(request, newReviewer(t, "cr-cascade@example.com")); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM comment WHERE id = $1::uuid`, request); err != nil {
			t.Fatalf("delete comment: %v", err)
		}

		if got := countReviewers(t, request); got != 0 {
			t.Errorf("%d reviewer rows survived their comment, want 0", got)
		}
	})

	// Unlike comment.author_id, which is SET NULL: authorship is a historical
	// fact, an assignment to a person who no longer exists is not.
	t.Run("deleting an account removes its assignments", func(t *testing.T) {
		request := newComment(t, "account-cascade", "pending")
		reviewer := newReviewer(t, "cr-account-cascade@example.com")

		if err := assign(request, reviewer); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, reviewer); err != nil {
			t.Fatalf("delete account: %v", err)
		}

		if got := countReviewers(t, request); got != 0 {
			t.Errorf("%d reviewer rows survived their account, want 0", got)
		}
	})

	// Deleting the document reaches through comment to here. No single
	// table's own test covers a break two levels up.
	t.Run("deleting the document cascades all the way to the reviewer", func(t *testing.T) {
		doc := newDoc(t, "Cascade Chain")

		var request string
		if err := pool.QueryRow(ctx,
			`INSERT INTO comment (document_id, body, review_status)
             VALUES ($1::uuid, 'please review', 'pending') RETURNING id::text`, doc,
		).Scan(&request); err != nil {
			t.Fatalf("create review request: %v", err)
		}
		if err := assign(request, newReviewer(t, "cr-chain@example.com")); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM document WHERE id = $1::uuid`, doc); err != nil {
			t.Fatalf("delete document: %v", err)
		}

		if got := countReviewers(t, request); got != 0 {
			t.Errorf("%d reviewer rows survived their document, want 0", got)
		}
	})

	// LAM-25 step 2's index, and the reason it is not redundant: the
	// composite primary key leads with comment_id, so the account direction
	// has nothing without it.
	t.Run("account_id leads an index and comment_id follows", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute lead
                     ON lead.attrelid = i.indrelid AND lead.attnum = i.indkey[0]
                   JOIN pg_attribute next
                     ON next.attrelid = i.indrelid AND next.attnum = i.indkey[1]
                  WHERE i.indrelid = 'comment_reviewer'::regclass
                    AND lead.attname = 'account_id'
                    AND next.attname = 'comment_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on comment_reviewer leads with account_id followed by comment_id, " +
				"so \"review requests assigned to me\" scans the table")
		}
	})

	t.Run("comment_id leads an index, via the primary key", func(t *testing.T) {
		var leads bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'comment_reviewer'::regclass
                    AND a.attname = 'comment_id'
             )`,
		).Scan(&leads); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !leads {
			t.Error("no index on comment_reviewer leads with comment_id, so the primary key " +
				"no longer covers the comment-side foreign key")
		}
	})
}

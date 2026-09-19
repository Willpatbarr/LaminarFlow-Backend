/*
╔═ index.go ════════════════════════════════════════════════════════════════════════════
║  search · the index write path
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      IndexDocument      func
║      ClearDocument      func
║      IndexTicket        func
║      IndexComment       func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/document.Save  →  ClearDocument then IndexDocument
║      Rebuild                 →  all four
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Every write takes a pgx.Tx rather than the pool, which is mechanism 3 of ADR 0001:
// an index write outside the caller's transaction is not expressible. Exported so
// internal/document can call them inside its own transaction - the tx parameter is
// what keeps that safe, not the casing.

/*
┌─ search ────────────────────────────────────────
│  removes one document's rows, before reindexing it
├─ in ────────────────────────────────────────────
│      ctx      context.Context
│      tx       pgx.Tx
│      docID    string
├─ out ───────────────────────────────────────────
│      error
*/

// ClearDocument exists because Save is delete-then-insert rather than an upsert: a
// field removed from the body has to lose its row. An upsert leaves it behind forever,
// and the rebuild then legitimately disagrees with the live index - a drift bug that
// presents as a rebuild bug.
func ClearDocument(ctx context.Context, tx pgx.Tx, docID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM search_index WHERE document_id = $1::uuid`, docID,
	); err != nil {
		return fmt.Errorf("search: clear document %s: %w", docID, err)
	}

	return nil
}

/*
┌─ search ────────────────────────────────────────
│  one row per body field, text extracted in Go
├─ in ────────────────────────────────────────────
│      ctx      context.Context
│      tx       pgx.Tx
│      docID    string
│      body     map[string]any     decoded JSON
├─ out ───────────────────────────────────────────
│      error
├─ example ───────────────────────────────────────
│      {"f_year": 2026}  →  one row, content "2026"
*/

func IndexDocument(ctx context.Context, tx pgx.Tx, docID string, body map[string]any) error {
	return indexDocument(ctx, tx, docID, body)
}

func indexDocument(ctx context.Context, tx pgx.Tx, docID string, body map[string]any) error {
	for fieldID, value := range body {
		// Scope and title are denormalised from the document rather than passed in,
		// so the index cannot disagree with the row it describes. workspace_id is
		// NOT NULL, so a missing document is a failed insert rather than a silently
		// unscoped row.
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_index
			     (document_id, field_id, content,
			      workspace_id, project_id, team_id, title_or_preview)
			 SELECT $1::uuid, $2, $3,
			        d.workspace_id, d.project_id, d.team_id, d.title
			   FROM document d
			  WHERE d.id = $1::uuid`,
			docID, fieldID, fieldText(value),
		); err != nil {
			return fmt.Errorf("search: index field %q: %w", fieldID, err)
		}
	}

	return nil
}

/*
┌─ search ────────────────────────────────────────
│  one row for a ticket's title and description
├─ in ────────────────────────────────────────────
│      ctx         context.Context
│      tx          pgx.Tx
│      ticketID    string
├─ out ───────────────────────────────────────────
│      error
├─ example ───────────────────────────────────────
│      "Fix login"  →  content "Fix login\n<desc>"
*/

// IndexTicket writes one row, not one per column. A ticket has no field structure to
// preserve - title and description are one searchable document, and splitting them
// would make a two-word phrase spanning both unfindable.
//
// field_id stays null. search_index_document_rows_carry_a_field is a biconditional, so
// a field_id here would fail the check rather than be ignored.
func IndexTicket(ctx context.Context, tx pgx.Tx, ticketID string) error {
	if _, err := tx.Exec(ctx, ticketInsert+` AND t.id = $1::uuid`, ticketID); err != nil {
		return fmt.Errorf("search: index ticket %s: %w", ticketID, err)
	}

	return nil
}

func indexAllTickets(ctx context.Context, tx pgx.Tx) (int, error) {
	tag, err := tx.Exec(ctx, ticketInsert)
	if err != nil {
		return 0, fmt.Errorf("search: index tickets: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// A ticket reaches its workspace through project and team, which is the chain LAM-26
// described. project_id and team_id are NOT NULL on the way, so every ticket row is
// fully scoped - unlike a document, whose narrower scopes may be null.
//
// The trailing WHERE true lets the single-ticket form append AND without a second
// copy of the statement, which is what keeps the live path and the rebuild identical.
const ticketInsert = `
	INSERT INTO search_index
	     (ticket_id, content, workspace_id, project_id, team_id, title_or_preview)
	SELECT t.id,
	       t.title || CASE WHEN t.description = '' THEN '' ELSE E'\n' || t.description END,
	       tm.workspace_id, t.project_id, p.team_id, t.title
	  FROM ticket t
	  JOIN project p ON p.id = t.project_id
	  JOIN team tm   ON tm.id = p.team_id
	 WHERE true`

/*
┌─ search ────────────────────────────────────────
│  one row for a comment, scoped through its parent
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx
│      commentID    string
├─ out ───────────────────────────────────────────
│      error
├─ example ───────────────────────────────────────
│      on a ticket  →  parent_reference "Fix login"
*/

// IndexComment carries parent_reference, which is what that column was added for: the
// UI renders "Comment on Fix login" without a join back to the parent.
//
// A comment hangs off exactly one of a ticket or a document - 0017 enforces that with
// num_nonnulls = 1 - so scope comes from whichever arm is set, and the two arms reach
// workspace by different routes. That is why this is a LEFT JOIN over both rather than
// one chain.
func IndexComment(ctx context.Context, tx pgx.Tx, commentID string) error {
	if _, err := tx.Exec(ctx, commentInsert+` AND c.id = $1::uuid`, commentID); err != nil {
		return fmt.Errorf("search: index comment %s: %w", commentID, err)
	}

	return nil
}

func indexAllComments(ctx context.Context, tx pgx.Tx) (int, error) {
	tag, err := tx.Exec(ctx, commentInsert)
	if err != nil {
		return 0, fmt.Errorf("search: index comments: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

// COALESCE picks whichever arm the comment uses. A document's project_id and team_id
// are nullable, so a document comment may be workspace-scoped only; a ticket comment
// is always fully scoped.
const commentInsert = `
	INSERT INTO search_index
	     (comment_id, content, workspace_id, project_id, team_id, title_or_preview,
	      parent_reference)
	SELECT c.id,
	       c.body,
	       COALESCE(tm.workspace_id, d.workspace_id),
	       COALESCE(t.project_id, d.project_id),
	       COALESCE(p.team_id, d.team_id),
	       c.body,
	       COALESCE(t.title, d.title)
	  FROM comment c
	  LEFT JOIN ticket t   ON t.id = c.ticket_id
	  LEFT JOIN project p  ON p.id = t.project_id
	  LEFT JOIN team tm    ON tm.id = p.team_id
	  LEFT JOIN document d ON d.id = c.document_id
	 WHERE true`

// documentBody is one document's id paired with its decoded body.
type documentBody struct {
	id   string
	body map[string]any
}

// readDocumentBodies loads every document before any insert runs. A pgx connection can
// only have one query in flight, so writing inside the rows.Next loop would fail on
// the same transaction.
func readDocumentBodies(ctx context.Context, tx pgx.Tx) ([]documentBody, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, body::text FROM document`)
	if err != nil {
		return nil, fmt.Errorf("search: read documents: %w", err)
	}

	docs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (documentBody, error) {
		var d documentBody
		var raw string
		if err := row.Scan(&d.id, &raw); err != nil {
			return documentBody{}, err
		}
		if err := json.Unmarshal([]byte(raw), &d.body); err != nil {
			return documentBody{}, fmt.Errorf("decode body %s: %w", d.id, err)
		}
		return d, nil
	})
	if err != nil {
		return nil, fmt.Errorf("search: read documents: %w", err)
	}

	return docs, nil
}

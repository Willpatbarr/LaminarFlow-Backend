/*
╔═ index.go ════════════════════════════════════════════════════════════════════════════
║  search · the index write path
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      IndexDocument      func
║      ClearDocument      func
║      IndexTicket        func
║      IndexComment       func
║      ClearComment       func
║      IndexProject       func
║      ClearProject       func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/document.Save  →  ClearDocument then IndexDocument
║      internal/project.Create →  IndexProject, inside its transaction
║      Rebuild                 →  all of them
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
	// A document with no fields still has to be findable by its title (LAM-100, 0037):
	// one row with no field_id, content the title. Only when the body is empty - with
	// fields, their rows already carry the title for matching.
	if len(body) == 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO search_index
			     (document_id, content, workspace_id, project_id, team_id, title_or_preview)
			 SELECT d.id, d.title, d.workspace_id, d.project_id, d.team_id, d.title
			   FROM document d
			  WHERE d.id = $1::uuid`,
			docID,
		); err != nil {
			return fmt.Errorf("search: index title of document %s: %w", docID, err)
		}

		return nil
	}

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
			docID, fieldID, FieldText(value),
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
	if _, err := indexTickets(ctx, tx, ` AND t.id = $1::uuid`, ticketID); err != nil {
		return fmt.Errorf("search: index ticket %s: %w", ticketID, err)
	}

	return nil
}

func indexAllTickets(ctx context.Context, tx pgx.Tx) (int, error) {
	n, err := indexTickets(ctx, tx, "")
	if err != nil {
		return 0, fmt.Errorf("search: index tickets: %w", err)
	}

	return n, nil
}

// A ticket reaches its workspace through project and team, which is the chain LAM-26
// described. project_id and team_id are NOT NULL on the way, so every ticket row is
// fully scoped - unlike a document, whose narrower scopes may be null.
//
// Archived tickets are excluded here, which is the half of 0029 that is easy to miss:
// archiving sets a column, and without this the ticket keeps answering searches. It
// is why internal/ticket calls ClearTicket on archive rather than only updating the
// row.
//
// The rows are read and then inserted from Go, because the description is Markdown and
// PlainText is Go (LAM-143). The trailing where lets the single-ticket form append AND
// without a second copy of the statement, which is what keeps the live path and the
// rebuild identical.
const ticketSelect = `
	SELECT t.id::text, t.title, t.description,
	       tm.workspace_id::text, t.project_id::text, p.team_id::text
	  FROM ticket t
	  JOIN project p ON p.id = t.project_id
	  JOIN team tm   ON tm.id = p.team_id
	 WHERE t.archived_at IS NULL`

const ticketInsert = `
	INSERT INTO search_index
	     (ticket_id, content, workspace_id, project_id, team_id, title_or_preview)
	VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5::uuid, $6)`

func indexTickets(ctx context.Context, tx pgx.Tx, where string, args ...any) (int, error) {
	rows, err := tx.Query(ctx, ticketSelect+where, args...)
	if err != nil {
		return 0, err
	}
	batch := &pgx.Batch{}
	for rows.Next() {
		var id, title, description, workspace, project, team string
		if err := rows.Scan(&id, &title, &description, &workspace, &project, &team); err != nil {
			rows.Close()

			return 0, err
		}
		content := title
		if description != "" {
			content += "\n" + PlainText(description)
		}
		batch.Queue(ticketInsert, id, content, workspace, project, team, title)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	return batch.Len(), sendBatch(ctx, tx, batch)
}

// sendBatch runs every queued statement and surfaces the first failure.
func sendBatch(ctx context.Context, tx pgx.Tx, batch *pgx.Batch) error {
	if batch.Len() == 0 {
		return nil
	}
	results := tx.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := results.Exec(); err != nil {
			results.Close()

			return err
		}
	}

	return results.Close()
}

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
	if _, err := indexComments(ctx, tx, ` AND c.id = $1::uuid`, commentID); err != nil {
		return fmt.Errorf("search: index comment %s: %w", commentID, err)
	}

	return nil
}

func indexAllComments(ctx context.Context, tx pgx.Tx) (int, error) {
	n, err := indexComments(ctx, tx, "")
	if err != nil {
		return 0, fmt.Errorf("search: index comments: %w", err)
	}

	return n, nil
}

// COALESCE picks whichever arm the comment uses. A document's project_id and team_id
// are nullable, so a document comment may be workspace-scoped only; a ticket comment
// is always fully scoped. The body is Markdown, so content and preview are its
// PlainText (LAM-143).
const commentSelect = `
	SELECT c.id::text,
	       c.body,
	       COALESCE(tm.workspace_id, d.workspace_id)::text,
	       COALESCE(t.project_id, d.project_id)::text,
	       COALESCE(p.team_id, d.team_id)::text,
	       COALESCE(t.title, d.title)
	  FROM comment c
	  LEFT JOIN ticket t   ON t.id = c.ticket_id
	  LEFT JOIN project p  ON p.id = t.project_id
	  LEFT JOIN team tm    ON tm.id = p.team_id
	  LEFT JOIN document d ON d.id = c.document_id
	 WHERE true`

const commentInsert = `
	INSERT INTO search_index
	     (comment_id, content, workspace_id, project_id, team_id, title_or_preview,
	      parent_reference)
	VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5::uuid, $2, $6)`

func indexComments(ctx context.Context, tx pgx.Tx, where string, args ...any) (int, error) {
	rows, err := tx.Query(ctx, commentSelect+where, args...)
	if err != nil {
		return 0, err
	}
	batch := &pgx.Batch{}
	for rows.Next() {
		var id, body string
		var workspace, project, team, parent *string
		if err := rows.Scan(&id, &body, &workspace, &project, &team, &parent); err != nil {
			rows.Close()

			return 0, err
		}
		batch.Queue(commentInsert, id, PlainText(body), workspace, project, team, parent)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	return batch.Len(), sendBatch(ctx, tx, batch)
}

/*
┌─ search ────────────────────────────────────────
│  removes one comment's row, before reindexing or deleting it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx
│      commentID    string
├─ out ───────────────────────────────────────────
│      error
*/

// ClearComment mirrors ClearTicket. An edit is clear-then-index, because the body is
// denormalised into the row (content and title_or_preview) and an edit that only inserted
// would leave the old text findable; a delete is clear alone. internal/comment calls it
// inside the transaction that writes the comment (LAM-99).
func ClearComment(ctx context.Context, tx pgx.Tx, commentID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM search_index WHERE comment_id = $1::uuid`, commentID,
	); err != nil {
		return fmt.Errorf("search: clear comment %s: %w", commentID, err)
	}

	return nil
}

// documentBody is one document's id paired with its decoded body.
type documentBody struct {
	id   string
	body map[string]any
}

// readDocumentBodies loads every live document before any insert runs. Archived ones are
// left out for the reason archived tickets are (0029, 0035): archiving removes the rows
// (document.Archive clears them), so a rebuild that put them back would disagree with the
// live index.
//
// It loads them all first because a pgx connection can
// only have one query in flight, so writing inside the rows.Next loop would fail on
// the same transaction.
func readDocumentBodies(ctx context.Context, tx pgx.Tx) ([]documentBody, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, body::text FROM document WHERE archived_at IS NULL`)
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

/*
┌─ search ────────────────────────────────────────
│  removes one ticket's row, before reindexing or archiving
├─ in ────────────────────────────────────────────
│      ctx         context.Context
│      tx          pgx.Tx
│      ticketID    string
├─ out ───────────────────────────────────────────
│      error
*/

// ClearTicket is what makes archiving actually remove a ticket from search, and what
// keeps an edited title from leaving the old one behind - title is denormalised here,
// so an update that does not reindex leaves search answering with stale text.
func ClearTicket(ctx context.Context, tx pgx.Tx, ticketID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM search_index WHERE ticket_id = $1::uuid`, ticketID,
	); err != nil {
		return fmt.Errorf("search: clear ticket %s: %w", ticketID, err)
	}

	return nil
}

/*
┌─ search ────────────────────────────────────────
│  one row for a project, its name
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx
│      projectID    string
├─ out ───────────────────────────────────────────
│      error
├─ example ───────────────────────────────────────
│      "Core"  →  content "Core"
*/

// IndexProject writes the row that makes a project findable by name (LAM-100). The
// source column is project_source_id, not project_id: project_id is the scope column and
// a project row is not scoped under a project (0037). team_id is set, project_id is
// not, which is the same shape a team-level document has.
//
// There is no archive for projects and no rename yet, so nothing calls ClearProject
// today. It exists so the day a rename lands, the clear-then-index pair every other
// source has is already there.
func IndexProject(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx, projectInsert+` AND p.id = $1::uuid`, projectID); err != nil {
		return fmt.Errorf("search: index project %s: %w", projectID, err)
	}

	return nil
}

func indexAllProjects(ctx context.Context, tx pgx.Tx) (int, error) {
	tag, err := tx.Exec(ctx, projectInsert)
	if err != nil {
		return 0, fmt.Errorf("search: index projects: %w", err)
	}

	return int(tag.RowsAffected()), nil
}

const projectInsert = `
	INSERT INTO search_index
	     (project_source_id, content, workspace_id, team_id, title_or_preview)
	SELECT p.id, p.name, tm.workspace_id, p.team_id, p.name
	  FROM project p
	  JOIN team tm ON tm.id = p.team_id
	 WHERE true`

/*
┌─ search ────────────────────────────────────────
│  removes one project's row, before reindexing it
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx
│      projectID    string
├─ out ───────────────────────────────────────────
│      error
*/

func ClearProject(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM search_index WHERE project_source_id = $1::uuid`, projectID,
	); err != nil {
		return fmt.Errorf("search: clear project %s: %w", projectID, err)
	}

	return nil
}

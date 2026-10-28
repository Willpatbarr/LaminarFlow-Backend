/*
╔═ links.go ════════════════════════════════════════════════════════════════════════════
║  ticket · links between tickets and documents (ticket_document)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      LinkedDocument  struct
║      LinkedTicket    struct
║      LinkResult      struct
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  link · unlink · get ticket · get document
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"fmt"
	"time"
)

// LinkedDocument is a document as a ticket lists it: enough to render a row, not the body.
type LinkedDocument struct {
	ID           string
	Title        string
	Type         string
	AspectTypeID *string
	ArchivedAt   *time.Time
}

// LinkedTicket is a ticket as a document lists it.
type LinkedTicket struct {
	ID         string
	Title      string
	ArchivedAt *time.Time
}

// LinkResult says what a link or unlink did. ProjectID is the ticket's, so the caller can
// find the workspace to announce in; Changed is false for an idempotent repeat, which
// writes nothing and so announces nothing.
type LinkResult struct {
	ProjectID string
	Changed   bool
}

/*
┌─ ticket ────────────────────────────────────────
│  links a ticket to a document, idempotently
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      ticketID     string
│      documentID   string
├─ out ───────────────────────────────────────────
│      LinkResult   Changed false when the pair was already linked
│      error        ErrNotFound: either end absent, or in a workspace the caller
│                   is not a member of, or the two in different workspaces
├─ example ───────────────────────────────────────
│      link twice  →  Changed true, then Changed false
*/

// Archived ends link too, unlike every other ticket statement: an archive must not sever
// links, so being able to make one while archived is the same rule. liveOnly is therefore
// absent here and from the other statements in this file, on purpose.
//
// The document is read here, not through internal/document: the check is one predicate
// (same workspace as the ticket) that has to be in the same statement as the insert, or a
// document moved between the check and the write would be linked across workspaces.
func (s *Service) LinkDocument(ctx context.Context, accountID, ticketID, documentID string) (LinkResult, error) {
	var project *string
	var inserted int
	err := s.pool.QueryRow(ctx,
		expand(`WITH ok AS (
		            SELECT t.id AS ticket_id, d.id AS document_id, t.project_id
		              FROM ticket t
		              JOIN document d ON d.workspace_id = (
		                   SELECT tm.workspace_id FROM project p JOIN team tm ON tm.id = p.team_id
		                    WHERE p.id = t.project_id)
		             WHERE t.id = $1::uuid AND d.id = $2::uuid AND `+callerCanReach+`),
		        ins AS (
		            INSERT INTO ticket_document (ticket_id, document_id, created_by)
		            SELECT ticket_id, document_id, $3::uuid FROM ok
		            ON CONFLICT DO NOTHING RETURNING 1)
		        SELECT (SELECT project_id::text FROM ok), (SELECT count(*) FROM ins)::int`, "$3"),
		ticketID, documentID, accountID,
	).Scan(&project, &inserted)
	if err != nil {
		return LinkResult{}, fmt.Errorf("ticket: link document: %w", err)
	}
	if project == nil {
		return LinkResult{}, ErrNotFound
	}

	return LinkResult{ProjectID: *project, Changed: inserted > 0}, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  removes a ticket's link to a document, idempotently
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      ticketID     string
│      documentID   string
├─ out ───────────────────────────────────────────
│      LinkResult   Changed false when there was no such link
│      error        ErrNotFound: the ticket is absent or not the caller's
├─ example ───────────────────────────────────────
│      unlink a pair never linked  →  Changed false, nil
*/

// Only the ticket is checked. A document id that is unknown, archived or foreign simply
// matches no row, which is the same answer as "never linked" - and since the link table is
// keyed by the ticket, nothing about another workspace's document is revealed.
func (s *Service) UnlinkDocument(ctx context.Context, accountID, ticketID, documentID string) (LinkResult, error) {
	var project *string
	var deleted int
	err := s.pool.QueryRow(ctx,
		expand(`WITH ok AS (
		            SELECT t.project_id FROM ticket t
		             WHERE t.id = $1::uuid AND `+callerCanReach+`),
		        del AS (
		            DELETE FROM ticket_document
		             WHERE ticket_id = $1::uuid AND document_id = $3::uuid
		               AND EXISTS (SELECT 1 FROM ok) RETURNING 1)
		        SELECT (SELECT project_id::text FROM ok), (SELECT count(*) FROM del)::int`, "$2"),
		ticketID, accountID, documentID,
	).Scan(&project, &deleted)
	if err != nil {
		return LinkResult{}, fmt.Errorf("ticket: unlink document: %w", err)
	}
	if project == nil {
		return LinkResult{}, ErrNotFound
	}

	return LinkResult{ProjectID: *project, Changed: deleted > 0}, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  a ticket's linked documents, archived ones included
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      ticketID     string
├─ out ───────────────────────────────────────────
│      []LinkedDocument   oldest link first; never nil
│      error              ErrNotFound: absent or not the caller's
*/
func (s *Service) LinkedDocuments(ctx context.Context, accountID, ticketID string) ([]LinkedDocument, error) {
	rows, err := s.pool.Query(ctx,
		expand(`SELECT d.id::text, d.title, d.type, d.aspect_type_id::text, d.archived_at
		          FROM ticket t
		          JOIN ticket_document td ON td.ticket_id = t.id
		          JOIN document d ON d.id = td.document_id
		         WHERE t.id = $1::uuid AND `+callerCanReach+`
		         ORDER BY td.created_at, d.id`, "$2"),
		ticketID, accountID)
	if err != nil {
		return nil, fmt.Errorf("ticket: linked documents: %w", err)
	}
	defer rows.Close()

	docs := []LinkedDocument{}
	for rows.Next() {
		var d LinkedDocument
		if err := rows.Scan(&d.ID, &d.Title, &d.Type, &d.AspectTypeID, &d.ArchivedAt); err != nil {
			return nil, fmt.Errorf("ticket: scan linked document: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket: linked documents: %w", err)
	}

	return docs, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  a document's linked tickets, archived ones included
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      documentID   string
├─ out ───────────────────────────────────────────
│      []LinkedTicket   oldest link first; never nil
│      error            fault only
*/

// The caller must reach the document too, but that is document.Get's check, made first by
// the one caller; here the tickets are scoped to the caller's workspaces on their own, so a
// missing check upstream would still leak nothing.
func (s *Service) LinkedTickets(ctx context.Context, accountID, documentID string) ([]LinkedTicket, error) {
	rows, err := s.pool.Query(ctx,
		expand(`SELECT t.id::text, t.title, t.archived_at
		          FROM ticket_document td
		          JOIN ticket t ON t.id = td.ticket_id
		         WHERE td.document_id = $1::uuid AND `+callerCanReach+`
		         ORDER BY td.created_at, t.id`, "$2"),
		documentID, accountID)
	if err != nil {
		return nil, fmt.Errorf("ticket: linked tickets: %w", err)
	}
	defer rows.Close()

	tickets := []LinkedTicket{}
	for rows.Next() {
		var t LinkedTicket
		if err := rows.Scan(&t.ID, &t.Title, &t.ArchivedAt); err != nil {
			return nil, fmt.Errorf("ticket: scan linked ticket: %w", err)
		}
		tickets = append(tickets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ticket: linked tickets: %w", err)
	}

	return tickets, nil
}

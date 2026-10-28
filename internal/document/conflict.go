package document

import "fmt"

// Conflict is the error for a conditional write that lost: the caller sent a version, and
// the document is no longer at it. It carries the server's copy, because the point of
// refusing instead of overwriting is to hand back what is there now, so a client holding
// unsynced edits can show both and let a person choose. Same reasoning, and same shape, as
// internal/ticket.Conflict.
//
// Archived separates the two things that can have happened - someone edited it, or someone
// removed it - and Current is the full row either way, archived or not, because "gone" is
// half of what a client needs to hear. Unlike a ticket, a document has no updated_by, so
// there is no ChangedBy; ChangedAt is updated_at, which an archive moves too.
type Conflict struct {
	Archived bool
	Current  Document
}

func (c *Conflict) Error() string {
	if c.Archived {
		return "document: archived since the version the caller saw"
	}

	return fmt.Sprintf("document: changed since the version the caller saw (now %d)", c.Current.Version)
}

// refuse decides, from the row a writer has locked, whether its write may proceed. It
// returns nil, ErrNotFound or a *Conflict.
//
// needsArchived is the state the verb works on: false for an update or an archive, true
// for an unarchive. A row in the wrong state is a conflict only when the caller sent a
// version and the row is archived - the single case where "archived" is news - and
// otherwise not found, which is what the same request without a version has always meant.
// Restoring a document that is not archived is not found either way.
func (d Document) refuse(ifVersion *int, needsArchived bool) error {
	archived := d.ArchivedAt != nil

	if archived != needsArchived {
		if ifVersion != nil && archived {
			return &Conflict{Archived: true, Current: d}
		}

		return ErrNotFound
	}

	if ifVersion != nil && d.Version != *ifVersion {
		return &Conflict{Current: d}
	}

	return nil
}

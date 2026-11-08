/*
╔═ folder.go ═══════════════════════════════════════════════════════════════════════════
║  document · folders: a project's documents filed in a tree (0053)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Folder                   struct
║      FolderPatch              struct
║      ErrFolderNotFound        error
║      ErrFolderCycle           error
║      ErrFolderNeedsProject    error
║      MaxFolderNameLength      const
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  /api/v1/document-folders
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package document

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/textlimit"

	"github.com/jackc/pgx/v5"
)

// Folders live in this package and not their own because deleting one re-files its
// documents, and only this package may write document (boundary_test.go).

var (
	// ErrFolderNotFound collapses absent and out-of-scope, as ErrNotFound does. As a
	// FieldError's Kind it is a folder_id or parent_id that names no folder of the project.
	ErrFolderNotFound = errors.New("document: folder not found")

	// ErrFolderCycle is a folder moved into itself or into one of its own sub-folders.
	// FieldError.Field is parent_id.
	ErrFolderCycle = errors.New("document: a folder cannot be moved into itself or its own sub-folder")

	// ErrFolderNeedsProject is a folder_id on a document that has no project: only a
	// project's documents are filed in folders. FieldError.Field is folder_id.
	ErrFolderNeedsProject = errors.New("document: only a project's documents can be filed in a folder")
)

// MaxFolderNameLength is the longest a folder's name may be, in characters.
const MaxFolderNameLength = 100

/*
┏━ Folder ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one folder as a caller sees it
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      ID             string
┃      ProjectID      string
┃      WorkspaceID    string      for the change feed
┃      ParentID       *string     null at the project's top level
┃      Name           string
┃      CreatedAt      time.Time
┃      UpdatedAt      time.Time
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      CreateFolder · GetFolder · ListFolders · UpdateFolder · DeleteFolder
*/

type Folder struct {
	ID          string
	ProjectID   string
	WorkspaceID string
	ParentID    *string
	Name        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FolderPatch is an update. A nil Name is left alone; MoveParent moves the folder under
// ParentID, or to the project's top level when ParentID is empty.
type FolderPatch struct {
	Name       *string
	MoveParent bool
	ParentID   string
}

// DeletedFolder is what deleting a folder did: the folder as it was, and everything it
// held, now one level up. The caller announces all of it after the commit.
type DeletedFolder struct {
	Folder    Folder
	Documents []Document
	Folders   []Folder
}

const folderColumns = `f.id::text, f.project_id::text,
	(SELECT tm.workspace_id::text FROM project pr JOIN team tm ON tm.id = pr.team_id WHERE pr.id = f.project_id),
	f.parent_id::text, f.name, f.created_at, f.updated_at`

func (f *Folder) dest() []any {
	return []any{&f.ID, &f.ProjectID, &f.WorkspaceID, &f.ParentID, &f.Name, &f.CreatedAt, &f.UpdatedAt}
}

// folderReachable scopes a folder to the caller's workspaces; $ACCOUNT is replaced.
const folderReachable = `EXISTS (
	        SELECT 1 FROM project pr
	          JOIN team tm ON tm.id = pr.team_id
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE pr.id = f.project_id)`

func scoped(query, accountPlaceholder string) string {
	return strings.ReplaceAll(query, "$ACCOUNT", accountPlaceholder)
}

// lockProjectFolders serialises every write that moves folders within one project, so two
// moves that would each be fine alone cannot together make a cycle.
func lockProjectFolders(ctx context.Context, tx pgx.Tx, projectID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('document_folder:' || $1::text, 0))`, projectID); err != nil {
		return fmt.Errorf("document: lock folders: %w", err)
	}

	return nil
}

// checkFolder is that folderID is a folder of projectID. Anything else is ErrFolderNotFound
// at field, so another project's folder is indistinguishable from none.
func checkFolder(ctx context.Context, tx pgx.Tx, projectID, folderID, field string) error {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM document_folder WHERE id = $1::uuid AND project_id = $2::uuid)`,
		folderID, projectID,
	).Scan(&ok); err != nil {
		return fmt.Errorf("document: read folder: %w", err)
	}
	if !ok {
		return &FieldError{Kind: ErrFolderNotFound, Field: field}
	}

	return nil
}

// CreateFolder makes a folder in a project the caller can reach, under parentID when it is
// not empty. A project the caller cannot reach is ErrScopeNotFound at project_id; a parent
// that is not a folder of the project is ErrFolderNotFound at parent_id.
func (s *Service) CreateFolder(ctx context.Context, accountID, projectID, parentID, name string) (Folder, error) {
	if err := textlimit.Required("name", name, MaxFolderNameLength); err != nil {
		return Folder{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Folder{}, fmt.Errorf("document: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var reachable bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM project pr
		                  JOIN team tm ON tm.id = pr.team_id
		                  JOIN workspace_member wm ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		                 WHERE pr.id = $1::uuid)`, projectID, accountID,
	).Scan(&reachable); err != nil {
		return Folder{}, fmt.Errorf("document: read project: %w", err)
	}
	if !reachable {
		return Folder{}, &FieldError{Kind: ErrScopeNotFound, Field: "project_id"}
	}

	if parentID != "" {
		if err := checkFolder(ctx, tx, projectID, parentID, "parent_id"); err != nil {
			return Folder{}, err
		}
	}

	var f Folder
	if err := tx.QueryRow(ctx,
		`WITH ins AS (
		     INSERT INTO document_folder (project_id, parent_id, name)
		     VALUES ($1::uuid, $2::uuid, $3)
		     RETURNING *)
		 SELECT `+folderColumns+` FROM ins f`,
		projectID, nilIfEmpty(parentID), name,
	).Scan(f.dest()...); err != nil {
		return Folder{}, fmt.Errorf("document: insert folder: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Folder{}, fmt.Errorf("document: commit: %w", err)
	}

	return f, nil
}

// GetFolder returns one folder the caller may see.
func (s *Service) GetFolder(ctx context.Context, accountID, id string) (Folder, error) {
	var f Folder
	err := s.pool.QueryRow(ctx,
		scoped(`SELECT `+folderColumns+` FROM document_folder f WHERE f.id = $1::uuid AND `+folderReachable, "$2"),
		id, accountID,
	).Scan(f.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Folder{}, ErrFolderNotFound
	}
	if err != nil {
		return Folder{}, fmt.Errorf("document: get folder: %w", err)
	}

	return f, nil
}

// ListFolders is every folder the caller can see, or only projectID's when it is not empty,
// by name. One page, always: a project's folders are a handful, and a client that holds the
// tree needs all of it at once.
func (s *Service) ListFolders(ctx context.Context, accountID, projectID string) ([]Folder, error) {
	rows, err := s.pool.Query(ctx,
		scoped(`SELECT `+folderColumns+` FROM document_folder f
		         WHERE ($2::uuid IS NULL OR f.project_id = $2::uuid) AND `+folderReachable+`
		         ORDER BY f.project_id, lower(f.name), f.id`, "$1"),
		accountID, nilIfEmpty(projectID),
	)
	if err != nil {
		return nil, fmt.Errorf("document: list folders: %w", err)
	}
	defer rows.Close()

	out := []Folder{}
	for rows.Next() {
		var f Folder
		if err := rows.Scan(f.dest()...); err != nil {
			return nil, fmt.Errorf("document: list folders scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("document: list folders rows: %w", err)
	}

	return out, nil
}

// lockFolder reads a folder FOR UPDATE within the caller's scope.
func lockFolder(ctx context.Context, tx pgx.Tx, accountID, id string) (Folder, error) {
	var f Folder
	err := tx.QueryRow(ctx,
		scoped(`SELECT `+folderColumns+` FROM document_folder f WHERE f.id = $1::uuid AND `+folderReachable+` FOR UPDATE OF f`, "$2"),
		id, accountID,
	).Scan(f.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Folder{}, ErrFolderNotFound
	}
	if err != nil {
		return Folder{}, fmt.Errorf("document: lock folder: %w", err)
	}

	return f, nil
}

// UpdateFolder renames a folder, moves it, or both. A move stays within the folder's project:
// a parent of another project is ErrFolderNotFound at parent_id, and a parent that is the
// folder itself or one of its sub-folders is ErrFolderCycle.
func (s *Service) UpdateFolder(ctx context.Context, accountID, id string, p FolderPatch) (Folder, error) {
	if p.Name != nil {
		if err := textlimit.Required("name", *p.Name, MaxFolderNameLength); err != nil {
			return Folder{}, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Folder{}, fmt.Errorf("document: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	cur, err := lockFolder(ctx, tx, accountID, id)
	if err != nil {
		return Folder{}, err
	}

	if p.MoveParent && p.ParentID != "" {
		if err := lockProjectFolders(ctx, tx, cur.ProjectID); err != nil {
			return Folder{}, err
		}
		if err := checkFolder(ctx, tx, cur.ProjectID, p.ParentID, "parent_id"); err != nil {
			return Folder{}, err
		}

		// Walk up from the new parent: meeting the folder on the way is a cycle.
		var cycle bool
		if err := tx.QueryRow(ctx,
			`WITH RECURSIVE up AS (
			     SELECT id, parent_id FROM document_folder WHERE id = $1::uuid
			     UNION ALL
			     SELECT f.id, f.parent_id FROM document_folder f JOIN up ON f.id = up.parent_id)
			 SELECT EXISTS (SELECT 1 FROM up WHERE id = $2::uuid)`,
			p.ParentID, id,
		).Scan(&cycle); err != nil {
			return Folder{}, fmt.Errorf("document: read folder ancestry: %w", err)
		}
		if cycle {
			return Folder{}, &FieldError{Kind: ErrFolderCycle, Field: "parent_id"}
		}
	}

	var f Folder
	if err := tx.QueryRow(ctx,
		`WITH upd AS (
		     UPDATE document_folder
		        SET name = coalesce($2::text, name),
		            parent_id = CASE WHEN $3::boolean THEN $4::uuid ELSE parent_id END,
		            updated_at = now()
		      WHERE id = $1::uuid
		  RETURNING *)
		 SELECT `+folderColumns+` FROM upd f`,
		id, p.Name, p.MoveParent, nilIfEmpty(p.ParentID),
	).Scan(f.dest()...); err != nil {
		return Folder{}, fmt.Errorf("document: update folder: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Folder{}, fmt.Errorf("document: commit: %w", err)
	}

	return f, nil
}

// DeleteFolder removes a folder and moves what it held - documents, archived ones included,
// and sub-folders - up into its parent (the project's top level for a top-level folder).
// Nothing but the folder is deleted. Each live document moved is a write like any other:
// its version rises, so a client holding an edit to it hears of the move.
func (s *Service) DeleteFolder(ctx context.Context, accountID, id string) (DeletedFolder, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeletedFolder{}, fmt.Errorf("document: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	f, err := lockFolder(ctx, tx, accountID, id)
	if err != nil {
		return DeletedFolder{}, err
	}
	if err := lockProjectFolders(ctx, tx, f.ProjectID); err != nil {
		return DeletedFolder{}, err
	}

	out := DeletedFolder{Folder: f}

	docs, err := tx.Query(ctx,
		`WITH d AS (
		     UPDATE document
		        SET folder_id = $2::uuid, updated_at = now(), version = version + 1
		      WHERE folder_id = $1::uuid
		  RETURNING *)
		 SELECT `+columns+` FROM d`,
		id, f.ParentID,
	)
	if err != nil {
		return DeletedFolder{}, fmt.Errorf("document: re-file documents: %w", err)
	}
	for docs.Next() {
		var d Document
		if err := d.scan(docs); err != nil {
			docs.Close()
			return DeletedFolder{}, fmt.Errorf("document: re-file scan: %w", err)
		}
		// An archived document moves too, so it comes back somewhere that exists, but it
		// is not announced: nobody holds it.
		if d.ArchivedAt == nil {
			out.Documents = append(out.Documents, d)
		}
	}
	docs.Close()
	if err := docs.Err(); err != nil {
		return DeletedFolder{}, fmt.Errorf("document: re-file rows: %w", err)
	}

	children, err := tx.Query(ctx,
		`WITH c AS (
		     UPDATE document_folder SET parent_id = $2::uuid, updated_at = now()
		      WHERE parent_id = $1::uuid
		  RETURNING *)
		 SELECT `+folderColumns+` FROM c f`,
		id, f.ParentID,
	)
	if err != nil {
		return DeletedFolder{}, fmt.Errorf("document: re-file folders: %w", err)
	}
	for children.Next() {
		var c Folder
		if err := children.Scan(c.dest()...); err != nil {
			children.Close()
			return DeletedFolder{}, fmt.Errorf("document: re-file folders scan: %w", err)
		}
		out.Folders = append(out.Folders, c)
	}
	children.Close()
	if err := children.Err(); err != nil {
		return DeletedFolder{}, fmt.Errorf("document: re-file folders rows: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM document_folder WHERE id = $1::uuid`, id); err != nil {
		return DeletedFolder{}, fmt.Errorf("document: delete folder: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return DeletedFolder{}, fmt.Errorf("document: commit: %w", err)
	}

	return out, nil
}

package document

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Document folders (0053): a project's tree, filing documents in it, and deleting a folder
// without deleting anything it held.

// folderProject makes a team and a project in ws and returns the project's id.
func folderProject(t *testing.T, pool *pgxpool.Pool, ws, name string) string {
	t.Helper()

	ctx := context.Background()
	var team, project string
	if err := pool.QueryRow(ctx,
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, ws, "Team "+name,
	).Scan(&team); err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (team_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, team, name,
	).Scan(&project); err != nil {
		t.Fatalf("project: %v", err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM team WHERE id = $1::uuid`, team) })

	return project
}

func mustFolder(t *testing.T, svc *Service, account, project, parent, name string) Folder {
	t.Helper()

	f, err := svc.CreateFolder(context.Background(), account, project, parent, name)
	if err != nil {
		t.Fatalf("create folder %q: %v", name, err)
	}

	return f
}

func folderOf(d Document) string { return strOrEmpty(d.FolderID) }

func TestFoldersNestWithinOneProject(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	me := member(t, pool, ws, "folder-nest@example.test")
	svc := NewService(pool)

	project := folderProject(t, pool, ws, "Folders nest")
	other := folderProject(t, pool, ws, "Folders other")

	specs := mustFolder(t, svc, me, project, "", "Specs")
	if specs.ParentID != nil || specs.ProjectID != project || specs.WorkspaceID != ws {
		t.Fatalf("top-level folder = %+v", specs)
	}
	api := mustFolder(t, svc, me, project, specs.ID, "API")
	if strOrEmpty(api.ParentID) != specs.ID {
		t.Fatalf("nested folder parent = %v, want %s", api.ParentID, specs.ID)
	}

	// A parent of another project is not found, as if it did not exist.
	_, err := svc.CreateFolder(ctx, me, other, specs.ID, "Elsewhere")
	wantFieldError(t, err, ErrFolderNotFound, "parent_id")

	// A blank name is refused.
	if _, err := svc.CreateFolder(ctx, me, project, "", "   "); err == nil {
		t.Fatal("blank folder name accepted")
	}

	// A project of a workspace the caller is not in is not found.
	stranger := member(t, pool, otherWorkspace(t, pool), "folder-stranger@example.test")
	_, err = svc.CreateFolder(ctx, stranger, project, "", "Mine")
	wantFieldError(t, err, ErrScopeNotFound, "project_id")
	if _, err := svc.GetFolder(ctx, stranger, specs.ID); !errors.Is(err, ErrFolderNotFound) {
		t.Fatalf("stranger get = %v, want not found", err)
	}

	listed, err := svc.ListFolders(ctx, me, project)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %v, %v; want both folders", listed, err)
	}
	if none, _ := svc.ListFolders(ctx, stranger, ""); len(none) != 0 {
		t.Fatalf("stranger sees %d folders", len(none))
	}
}

func TestAFolderCannotMoveIntoItself(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	me := member(t, pool, ws, "folder-cycle@example.test")
	svc := NewService(pool)
	project := folderProject(t, pool, ws, "Folders cycle")

	a := mustFolder(t, svc, me, project, "", "A")
	b := mustFolder(t, svc, me, project, a.ID, "B")
	c := mustFolder(t, svc, me, project, b.ID, "C")

	for _, target := range []Folder{a, c} {
		_, err := svc.UpdateFolder(ctx, me, a.ID, FolderPatch{MoveParent: true, ParentID: target.ID})
		wantFieldError(t, err, ErrFolderCycle, "parent_id")
	}

	// Moving C to the top level and renaming it is fine; a omitted parent is left alone.
	moved, err := svc.UpdateFolder(ctx, me, c.ID, FolderPatch{MoveParent: true})
	if err != nil || moved.ParentID != nil {
		t.Fatalf("move to top = %+v, %v", moved, err)
	}
	name := "Renamed"
	renamed, err := svc.UpdateFolder(ctx, me, b.ID, FolderPatch{Name: &name})
	if err != nil || renamed.Name != name || strOrEmpty(renamed.ParentID) != a.ID {
		t.Fatalf("rename = %+v, %v; want parent kept", renamed, err)
	}

	// And A can now go inside C, which is no longer below it.
	if _, err := svc.UpdateFolder(ctx, me, a.ID, FolderPatch{MoveParent: true, ParentID: c.ID}); err != nil {
		t.Fatalf("move A into C: %v", err)
	}
}

func TestDocumentsAreFiledInTheirProjectsFolders(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	me := member(t, pool, ws, "folder-file@example.test")
	svc := NewService(pool)
	project := folderProject(t, pool, ws, "Folders file")
	other := folderProject(t, pool, ws, "Folders file other")

	specs := mustFolder(t, svc, me, project, "", "Specs")
	theirs := mustFolder(t, svc, me, other, "", "Theirs")

	d, err := svc.Write(ctx, SaveParams{AccountID: me, ProjectID: project, FolderID: specs.ID, Title: "In specs"})
	if err != nil || folderOf(d) != specs.ID {
		t.Fatalf("create in folder = %v, %v", d.FolderID, err)
	}

	// A folder of another project, or no project at all, is refused.
	_, err = svc.Write(ctx, SaveParams{AccountID: me, ProjectID: project, FolderID: theirs.ID})
	wantFieldError(t, err, ErrFolderNotFound, "folder_id")
	_, err = svc.Write(ctx, SaveParams{AccountID: me, FolderID: specs.ID})
	wantFieldError(t, err, ErrFolderNeedsProject, "folder_id")

	// An edit that does not name the folder leaves it alone.
	d, err = svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: "Still in specs"})
	if err != nil || folderOf(d) != specs.ID {
		t.Fatalf("plain edit folder = %v, %v", d.FolderID, err)
	}

	// Out to the top level, and back in.
	d, err = svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveFolder: true})
	if err != nil || d.FolderID != nil {
		t.Fatalf("to top level = %v, %v", d.FolderID, err)
	}
	d, err = svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveFolder: true, FolderID: specs.ID})
	if err != nil || folderOf(d) != specs.ID {
		t.Fatalf("back in = %v, %v", d.FolderID, err)
	}
	_, err = svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveFolder: true, FolderID: theirs.ID})
	wantFieldError(t, err, ErrFolderNotFound, "folder_id")

	// Moving project takes it out of its folder, unless the move names one of the new project's.
	moved, err := svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveProject: true, ProjectID: other})
	if err != nil || moved.FolderID != nil || strOrEmpty(moved.ProjectID) != other {
		t.Fatalf("project move = %+v, %v; want out of its folder", moved, err)
	}
	moved, err = svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveProject: true, ProjectID: project,
		MoveFolder: true, FolderID: specs.ID})
	if err != nil || folderOf(moved) != specs.ID {
		t.Fatalf("project move into folder = %v, %v", moved.FolderID, err)
	}

	// Re-sending the same project is not a move, so the folder stays.
	same, err := svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveProject: true, ProjectID: project})
	if err != nil || folderOf(same) != specs.ID {
		t.Fatalf("same project = %v, %v; want folder kept", same.FolderID, err)
	}

	// Taking it out of its project takes it out of the folder too.
	teamWide, err := svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveProject: true})
	if err != nil || teamWide.FolderID != nil {
		t.Fatalf("team-wide = %v, %v", teamWide.FolderID, err)
	}
}

func TestDeletingAFolderMovesWhatItHeldUpALevel(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	me := member(t, pool, ws, "folder-delete@example.test")
	svc := NewService(pool)
	project := folderProject(t, pool, ws, "Folders delete")

	outer := mustFolder(t, svc, me, project, "", "Outer")
	middle := mustFolder(t, svc, me, project, outer.ID, "Middle")
	inner := mustFolder(t, svc, me, project, middle.ID, "Inner")

	live, err := svc.Write(ctx, SaveParams{AccountID: me, ProjectID: project, FolderID: middle.ID, Title: "Live"})
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	gone, err := svc.Write(ctx, SaveParams{AccountID: me, ProjectID: project, FolderID: middle.ID, Title: "Archived"})
	if err != nil {
		t.Fatalf("archived: %v", err)
	}
	if gone, err = svc.Archive(ctx, me, gone.ID, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}

	deleted, err := svc.DeleteFolder(ctx, me, middle.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.Folder.ID != middle.ID {
		t.Errorf("deleted = %s, want middle", deleted.Folder.ID)
	}
	if len(deleted.Documents) != 1 || deleted.Documents[0].ID != live.ID ||
		folderOf(deleted.Documents[0]) != outer.ID || deleted.Documents[0].Version != live.Version+1 {
		t.Errorf("re-filed documents = %+v; want the live one in outer at the next version", deleted.Documents)
	}
	if len(deleted.Folders) != 1 || deleted.Folders[0].ID != inner.ID || strOrEmpty(deleted.Folders[0].ParentID) != outer.ID {
		t.Errorf("re-filed folders = %+v; want inner under outer", deleted.Folders)
	}

	// The archived document moved too, so restoring it lands somewhere real.
	var archivedFolder *string
	if err := pool.QueryRow(ctx, `SELECT folder_id::text FROM document WHERE id = $1::uuid`, gone.ID).Scan(&archivedFolder); err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if strOrEmpty(archivedFolder) != outer.ID {
		t.Errorf("archived document folder = %v, want outer", archivedFolder)
	}

	if _, err := svc.GetFolder(ctx, me, middle.ID); !errors.Is(err, ErrFolderNotFound) {
		t.Errorf("get deleted folder = %v, want not found", err)
	}

	// Deleting a top-level folder drops its contents to the project's top level.
	top, err := svc.DeleteFolder(ctx, me, outer.ID)
	if err != nil {
		t.Fatalf("delete outer: %v", err)
	}
	if len(top.Documents) != 1 || top.Documents[0].FolderID != nil {
		t.Errorf("top-level delete documents = %+v, want at the top level", top.Documents)
	}
	if len(top.Folders) != 1 || top.Folders[0].ParentID != nil {
		t.Errorf("top-level delete folders = %+v, want at the top level", top.Folders)
	}
}

func TestTheSchemaKeepsFoldersHonest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	ws := defaultWorkspace(t, pool)
	me := member(t, pool, ws, "folder-schema@example.test")
	svc := NewService(pool)
	project := folderProject(t, pool, ws, "Folders schema")
	other := folderProject(t, pool, ws, "Folders schema other")

	parent := mustFolder(t, svc, me, project, "", "Parent")
	child := mustFolder(t, svc, me, project, parent.ID, "Child")
	theirs := mustFolder(t, svc, me, other, "", "Theirs")

	// A document cannot point at another project's folder, even written directly.
	d, err := svc.Write(ctx, SaveParams{AccountID: me, ProjectID: project, Title: "Direct"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE document SET folder_id = $1::uuid WHERE id = $2::uuid`, theirs.ID, d.ID); err == nil {
		t.Error("document filed in another project's folder")
	}

	// A folder with sub-folders cannot be deleted directly: that would lose a subtree.
	if _, err := pool.Exec(ctx, `DELETE FROM document_folder WHERE id = $1::uuid`, parent.ID); err == nil {
		t.Error("folder with a sub-folder deleted directly")
	}

	// Deleting the project deletes its folders and leaves its documents with no folder.
	if _, err := svc.Write(ctx, SaveParams{AccountID: me, ID: d.ID, Title: d.Title, MoveFolder: true, FolderID: child.ID}); err != nil {
		t.Fatalf("file: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, project); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var folder, proj *string
	if err := pool.QueryRow(ctx, `SELECT folder_id::text, project_id::text FROM document WHERE id = $1::uuid`, d.ID).Scan(&folder, &proj); err != nil {
		t.Fatalf("read: %v", err)
	}
	if folder != nil || proj != nil {
		t.Errorf("after project delete folder = %v, project = %v; want both null", folder, proj)
	}
}

// otherWorkspace makes a workspace nobody from the default one belongs to.
func otherWorkspace(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name) VALUES ('Folder stranger') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1::uuid`, id) })

	return id
}

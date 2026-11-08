package api

import (
	"net/http"
	"testing"
)

// Document folders on the wire: the codes, where a 422 points, and what deleting a folder
// announces. The rules themselves are tested in internal/document.

type folderWire struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	ParentID  *string `json:"parent_id"`
	Name      string  `json:"name"`
}

func (f feed) folder(t *testing.T, cookie string, body map[string]any) folderWire {
	t.Helper()

	rec := call(t, f.mux, http.MethodPost, V1+"/document-folders", body, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("create folder: status %d, body %s", rec.Code, rec.Body)
	}
	var out folderWire
	decode(t, rec, &out)

	return out
}

func TestFoldersRoundTripAndRefuseWhatTheyShould(t *testing.T) {
	f := newFeed(t)

	specs := f.folder(t, f.willC, map[string]any{"project_id": f.project.ID, "name": "Specs"})
	if specs.ProjectID != f.project.ID || specs.ParentID != nil || specs.Name != "Specs" {
		t.Fatalf("created = %+v", specs)
	}
	api := f.folder(t, f.willC, map[string]any{"project_id": f.project.ID, "parent_id": specs.ID, "name": "API"})

	// Someone in another workspace sees neither.
	rec := call(t, f.mux, http.MethodGet, V1+"/document-folders/"+specs.ID, nil, f.carolC)
	wantDocProblem(t, rec, http.StatusNotFound, CodeFolderNotFound, "")

	rec = call(t, f.mux, http.MethodPost, V1+"/document-folders/list", map[string]any{"project_id": f.project.ID}, f.sarahC)
	var list struct {
		Items []folderWire `json:"items"`
	}
	decode(t, rec, &list)
	if rec.Code != http.StatusOK || len(list.Items) != 2 {
		t.Fatalf("list: %d %+v", rec.Code, list.Items)
	}

	// Moving Specs into its own sub-folder is a cycle.
	rec = call(t, f.mux, http.MethodPut, V1+"/document-folders/"+specs.ID, map[string]any{"parent_id": api.ID}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeFolderCycle, "body.parent_id")

	// null moves API to the top level; an omitted name is kept.
	rec = call(t, f.mux, http.MethodPut, V1+"/document-folders/"+api.ID, map[string]any{"parent_id": nil}, f.willC)
	var moved folderWire
	decode(t, rec, &moved)
	if rec.Code != http.StatusOK || moved.ParentID != nil || moved.Name != "API" {
		t.Fatalf("move to top: %d %+v", rec.Code, moved)
	}

	// A document with no project cannot be filed.
	rec = call(t, f.mux, http.MethodPost, V1+"/documents", map[string]any{"title": "Loose", "folder_id": specs.ID}, f.willC)
	wantDocProblem(t, rec, http.StatusUnprocessableEntity, CodeFolderNeedsProject, "body.folder_id")

	d := f.document(t, f.willC, map[string]any{"title": "Filed", "project_id": f.project.ID, "folder_id": specs.ID})
	if d.FolderID == nil || *d.FolderID != specs.ID {
		t.Fatalf("filed document folder = %v", d.FolderID)
	}

	// PUT without folder_id keeps it; with it, moves it.
	rec = call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "Filed 2"}, f.willC)
	var got documentWire
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.FolderID == nil || *got.FolderID != specs.ID {
		t.Fatalf("plain put: %d folder %v", rec.Code, got.FolderID)
	}
	rec = call(t, f.mux, http.MethodPut, V1+"/documents/"+d.ID, map[string]any{"title": "Filed 2", "folder_id": api.ID}, f.willC)
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.FolderID == nil || *got.FolderID != api.ID {
		t.Fatalf("move put: %d folder %v", rec.Code, got.FolderID)
	}
}

func TestDeletingAFolderAnnouncesWhatMoved(t *testing.T) {
	f := newFeed(t)

	outer := f.folder(t, f.willC, map[string]any{"project_id": f.project.ID, "name": "Outer"})
	inner := f.folder(t, f.willC, map[string]any{"project_id": f.project.ID, "parent_id": outer.ID, "name": "Inner"})
	sub := f.folder(t, f.willC, map[string]any{"project_id": f.project.ID, "parent_id": inner.ID, "name": "Sub"})
	d := f.document(t, f.willC, map[string]any{"title": "Inside", "project_id": f.project.ID, "folder_id": inner.ID})

	sarah := f.open(t, f.cookie(f.sarahC))

	rec := call(t, f.mux, http.MethodDelete, V1+"/document-folders/"+inner.ID, nil, f.willC)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}

	ev := readEvent(t, sarah)
	if ev.Type != "document.updated" || ev.DocumentID != d.ID || ev.Version != d.Version+1 || ev.Document["folder_id"] != outer.ID {
		t.Errorf("first event = %s %s v%d folder %v; want the document, one version on, in outer",
			ev.Type, ev.DocumentID, ev.Version, ev.Document["folder_id"])
	}
	ev = readEvent(t, sarah)
	if ev.Type != "document_folder.updated" || ev.FolderID != sub.ID || ev.Folder["parent_id"] != outer.ID {
		t.Errorf("second event = %s %s parent %v; want sub under outer", ev.Type, ev.FolderID, ev.Folder["parent_id"])
	}
	ev = readEvent(t, sarah)
	if ev.Type != "document_folder.deleted" || ev.FolderID != inner.ID || ev.ProjectID != f.project.ID || ev.Folder["id"] != inner.ID {
		t.Errorf("third event = %s %s; want inner deleted", ev.Type, ev.FolderID)
	}

	rec = call(t, f.mux, http.MethodDelete, V1+"/document-folders/"+inner.ID, nil, f.willC)
	wantDocProblem(t, rec, http.StatusNotFound, CodeFolderNotFound, "")
}

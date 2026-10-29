package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/export"
)

// LAM-102. POST /export over the wire: session only, a zip of JSON files and Markdown
// documents, and no credential in it.

func zipFiles(t *testing.T, b []byte) map[string][]byte {
	t.Helper()

	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}

	return out
}

func TestExportIsASessionOnlyZipOfJSONAndMarkdown(t *testing.T) {
	f := newFeed(t)

	tk := f.ticketWith(t, "Ship the export", "")
	normal := f.document(t, f.willC, map[string]any{"title": "Meeting notes", "project_id": f.project.ID, "body": map[string]any{"content": "# Agenda\n\n- one"}})
	typeID, fields := f.aspectType(t, f.willC, "Goal", "Risks")
	aspect := f.document(t, f.willC, map[string]any{"title": "Plan", "project_id": f.project.ID, "type": "aspect", "aspect_type_id": typeID,
		"body": map[string]any{fields[0]: "Make it **work**", fields[1]: "None"}})
	f.attach(t, f.willC, upload{fields: ownedBy(tk.ID), filename: "shot.png", contentType: "image/png", data: tinyPNG})

	// 401 with no credential, 403 for any token, even one holding every scope.
	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/export", nil, ""), http.StatusUnauthorized, "unauthorized")
	tok := createToken(t, f.mux, f.willC, "ticket:read", "ticket:write", "aspect:read")
	assertCode(t, call(t, f.mux, http.MethodPost, V1+"/export", nil, "", bearer(tok.Token)...), http.StatusForbidden, "insufficient_scope")

	rec := call(t, f.mux, http.MethodPost, V1+"/export", nil, f.willC)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %.300s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="laminarflow-export-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	files := zipFiles(t, rec.Body.Bytes())

	var m export.Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.SchemaVersion < 44 || m.Documents != 2 || m.Files["tickets.json"] != 1 || m.Files["attachments.json"] != 1 {
		t.Errorf("manifest = %+v", m)
	}
	for _, name := range []string{"tickets.json", "documents.json", "teams.json", "projects.json", "settings.json", "aspect_types.json", "aspect_type_fields.json", "attachments.json", "accounts.json"} {
		if files[name] == nil {
			t.Errorf("missing %s; have %v", name, keys(files))
		}
	}

	if !strings.Contains(string(files["tickets.json"]), "Ship the export") {
		t.Errorf("tickets.json does not hold the ticket")
	}
	// Metadata only: the body is the Markdown file's job.
	if strings.Contains(string(files["documents.json"]), "Agenda") || !strings.Contains(string(files["documents.json"]), normal.ID) {
		t.Errorf("documents.json = %.400s", files["documents.json"])
	}

	if got := string(files["documents/"+normal.ID+".md"]); got != "# Meeting notes\n\n# Agenda\n\n- one\n" {
		t.Errorf("normal document = %q", got)
	}
	if got := string(files["documents/"+aspect.ID+".md"]); got != "# Plan\n\n## Goal\n\nMake it **work**\n\n## Risks\n\nNone\n" {
		t.Errorf("aspect document = %q", got)
	}

	// Credentials never leave.
	for _, name := range []string{"api_tokens.json", "sessions.json", "search_indexes.json"} {
		if files[name] != nil {
			t.Errorf("%s was exported", name)
		}
	}
	all := ""
	for _, b := range files {
		all += string(b)
	}
	if strings.Contains(all, "password_hash") || strings.Contains(all, "token_hash") || strings.Contains(all, tok.Token) {
		t.Errorf("a credential is in the export")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}

	return out
}

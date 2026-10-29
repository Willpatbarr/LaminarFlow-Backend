// Package export writes the whole workspace as a zip of plain files: one JSON file per
// table and one Markdown file per document. It is the "take your data and leave" format
// (Master Spec §10, §14), for people and scripts; it is not a backup. A backup is
// scripts/backup.sh, which captures the database and the attachment bytes exactly.
//
// Everything is read in one REPEATABLE READ, read-only transaction, so the files agree
// with each other even while the app is being written to.
package export

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// skipTables are never exported. Credentials and live sessions are not data a person
// takes with them (exporting them would turn the zip into something to guard like the
// database), and search_index is derived: `reindex` rebuilds it from the documents.
// schema_migrations is carried in the manifest as a single version instead.
var skipTables = map[string]bool{
	"schema_migrations": true,
	"search_index":      true,
	"session":           true,
	"api_token":         true,
}

// dropColumns are removed from a table that is exported. A password hash is a credential
// in the same way a token is.
var dropColumns = map[string][]string{
	"account":  {"password_hash"},
	"document": {"body"}, // written to documents/<id>.md instead
}

// Manifest describes one export. It is the first thing to read and is written last, so
// its counts are of what the zip actually holds.
type Manifest struct {
	ExportedAt    time.Time      `json:"exported_at"`
	SchemaVersion int            `json:"schema_version"`
	Files         map[string]int `json:"files" doc:"rows per JSON file"`
	Documents     int            `json:"documents" doc:"Markdown files under documents/"`
	Notes         []string       `json:"notes"`
}

// Service reads the export.
type Service struct {
	pool    *pgxpool.Pool
	docs    *document.Service
	aspects *aspect.Service
}

func NewService(pool *pgxpool.Pool, docs *document.Service, aspects *aspect.Service) *Service {
	return &Service{pool: pool, docs: docs, aspects: aspects}
}

// Write streams the zip to w. A failure part-way leaves a truncated zip, so a caller that
// can still report an error (the HTTP handler) writes to a buffer first.
func (s *Service) Write(ctx context.Context, w io.Writer) (Manifest, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Manifest{}, fmt.Errorf("export: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; nothing to lose

	now := time.Now().UTC().Truncate(time.Second)
	m := Manifest{ExportedAt: now, Files: map[string]int{}, Notes: []string{
		"Attachment file contents are not in this export, only their records (attachments.json). Use scripts/backup.sh to capture the files.",
		"Document bodies are Markdown under documents/<id>.md. Aspect documents have one section per field, headed by the field's label.",
		"Passwords, API tokens and sessions are never exported. search_index is derived and rebuilt by `reindex`.",
	}}

	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&m.SchemaVersion); err != nil {
		return Manifest{}, fmt.Errorf("export: schema version: %w", err)
	}

	zw := zip.NewWriter(w)
	add := func(name string, data []byte) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}

	tables, err := tableNames(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}

	for _, t := range tables {
		raw, n, err := dumpTable(ctx, tx, t)
		if err != nil {
			return Manifest{}, err
		}

		name := plural(t) + ".json"
		pretty, err := indent(raw)
		if err != nil {
			return Manifest{}, fmt.Errorf("export: %s: %w", name, err)
		}
		if err := add(name, pretty); err != nil {
			return Manifest{}, fmt.Errorf("export: write %s: %w", name, err)
		}
		m.Files[name] = n
	}

	docs, err := s.docs.Export(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	labels, err := s.aspects.AllFields(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	for _, d := range docs {
		if err := add("documents/"+d.ID+".md", []byte(Markdown(d, labels))); err != nil {
			return Manifest{}, fmt.Errorf("export: write document %s: %w", d.ID, err)
		}
	}
	m.Documents = len(docs)

	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err := add("manifest.json", append(body, '\n')); err != nil {
		return Manifest{}, err
	}
	if err := zw.Close(); err != nil {
		return Manifest{}, fmt.Errorf("export: close zip: %w", err)
	}

	return m, nil
}

func tableNames(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("export: list tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if !skipTables[n] {
			out = append(out, n)
		}
	}

	return out, rows.Err()
}

// dumpTable returns the table as a JSON array, ordered by each row's own text so the
// output is the same twice, and the row count. The table name comes from the catalogue
// and is quoted as an identifier regardless.
func dumpTable(ctx context.Context, tx pgx.Tx, table string) ([]byte, int, error) {
	drop := dropColumns[table]
	if drop == nil {
		drop = []string{}
	}

	var (
		raw string
		n   int
	)
	err := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(t) - $1::text[] ORDER BY t::text), '[]'::jsonb)::text, count(*)
		FROM `+pgx.Identifier{table}.Sanitize()+` t`, drop).Scan(&raw, &n)
	if err != nil {
		return nil, 0, fmt.Errorf("export: read %s: %w", table, err)
	}

	return []byte(raw), n, nil
}

func indent(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(out, '\n'), nil
}

func plural(t string) string {
	switch {
	case strings.HasSuffix(t, "s"):
		return t + "es"
	case strings.HasSuffix(t, "y"):
		return strings.TrimSuffix(t, "y") + "ies"
	}

	return t + "s"
}

// Labels maps an aspect type id to its fields in display order.
type Labels map[string][]aspect.Field

// Markdown renders a document as one file. A normal document is its title and its
// content. An aspect document is its title and one "## label" section per field, in the
// type's field order. A body key that is no longer a field of the type (a field removed
// after values were written) is kept under its raw id rather than dropped: an export
// that silently loses text is worse than an untidy heading.
func Markdown(d document.Document, labels Labels) string {
	var b strings.Builder

	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = "Untitled"
	}
	b.WriteString("# " + oneLine(title) + "\n")

	text := func(key string) string {
		s, _ := d.Body[key].(string)
		return strings.TrimSpace(s)
	}

	if d.AspectTypeID == nil {
		if s := text(document.ContentKey); s != "" {
			b.WriteString("\n" + s + "\n")
		}
		return b.String()
	}

	seen := map[string]bool{}
	section := func(heading, key string) {
		seen[key] = true
		b.WriteString("\n## " + oneLine(heading) + "\n")
		if s := text(key); s != "" {
			b.WriteString("\n" + s + "\n")
		}
	}

	for _, f := range labels[*d.AspectTypeID] {
		section(f.Label, f.ID)
	}

	var rest []string
	for k := range d.Body {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if _, ok := d.Body[k].(string); ok {
			section(k, k)
		}
	}

	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

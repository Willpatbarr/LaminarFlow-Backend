/*
╔═ sqlguard.go ═════════════════════════════════════════════════════════════════════════
║  test support · table ownership
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Rule           struct
║      Assert         func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/document  →  document, reader internal/search
║      internal/search    →  search_index
║      internal/auth      →  api_token · session
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package sqlguard fails a test when SQL against an owned table appears outside the
// package that owns it.
//
// Go's encapsulation is package-scoped, so an unexported pool stops a caller reaching
// through a service but not a new package calling db.Connect and writing the table
// itself. See docs/adr/0001-write-path-enforcement.md.
package sqlguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Skipped by every rule: a schema constraint test proves a constraint by violating it,
// so naming the table is the technique. Shrinks as tables gain owners - api_token has
// one now, so schema_api_token_test.go belongs in internal/auth.
const schemaTestDir = "migrate"

// Writes are never allowed outside the owner. Drift is a write problem, and this half
// of the guard has no exceptions at all.
var writeVerbs = regexp.MustCompile(
	`(?i)\b(insert\s+into|update|delete\s+from)\s+"?(%TABLES%)\b`)

// Reads are allowed to the owner and to any package the rule names a reader.
//
// ADR 0001 guards reads as well as writes, because a raw SELECT elsewhere would bypass
// the workspace scoping Save enforces. That reasoning does not reach a package whose
// job is to denormalise those very columns: internal/search reads document, ticket and
// comment precisely so each index row carries the scope of the thing it describes, and
// its rebuild is instance-wide admin work by design. So reads are grantable and writes
// are not.
var readVerbs = regexp.MustCompile(
	`(?i)\b(from|join)\s+"?(%TABLES%)\b`)

/*
┏━ Rule ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  who may touch a set of tables, and how
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Owner      string      directory name, reads and writes
┃      Tables     []string    at least one
┃      Readers    []string    may read, never write
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      each owning package's boundary test
*/

type Rule struct {
	Owner   string
	Tables  []string
	Readers []string
}

/*
┌─ sqlguard ──────────────────────────────────────
│  fails tb on SQL that breaks a table's ownership
├─ in ────────────────────────────────────────────
│      tb    testing.TB
│      r     Rule
├─ example ───────────────────────────────────────
│      owner auth, api_token  →  fails on internal/db
*/

func Assert(tb testing.TB, r Rule) {
	tb.Helper()

	if len(r.Tables) == 0 {
		tb.Fatal("sqlguard.Assert called with no tables, so it would assert nothing")
	}
	if r.Owner == "" {
		tb.Fatal("sqlguard.Assert called with no owner")
	}

	root, err := moduleRoot()
	if err != nil {
		tb.Fatalf("locate module root: %v", err)
	}

	alt := strings.Join(r.Tables, "|")
	writes := recompile(writeVerbs, alt)
	reads := recompile(readVerbs, alt)

	readers := map[string]bool{}
	for _, d := range r.Readers {
		readers[d] = true
	}

	fset := token.NewFileSet()
	type offence struct{ where, kind string }
	var offences []offence

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == r.Owner || d.Name() == schemaTestDir {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

		// A reader is exempt from the read half only. Its directory is still
		// walked, so a write from inside it is still caught.
		isReader := readers[filepath.Base(filepath.Dir(path))]

		// Mode 0 drops comments, so prose naming a table is never a failure.
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}

			switch {
			case writes.MatchString(s):
				offences = append(offences, offence{fset.Position(lit.Pos()).String(), "writes"})
			case !isReader && reads.MatchString(s):
				offences = append(offences, offence{fset.Position(lit.Pos()).String(), "reads"})
			}
			return true
		})
		return nil
	})
	if err != nil {
		tb.Fatalf("walk: %v", err)
	}

	for _, o := range offences {
		tb.Errorf("SQL %s %s outside package %s: %s\n"+
			"  route it through that package's service - it is the only writer\n"+
			"  see docs/adr/0001-write-path-enforcement.md",
			o.kind, strings.Join(r.Tables, "/"), r.Owner, o.where)
	}
}

// recompile substitutes the table alternation into a verb pattern. The patterns carry
// a placeholder rather than being built from scratch per call so the verb lists stay
// declared once, where their reasoning is written.
func recompile(pattern *regexp.Regexp, alternation string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(pattern.String(), "%TABLES%", alternation))
}

/*
┌─ sqlguard ──────────────────────────────────────
│  walks up to the directory holding go.mod
├─ out ───────────────────────────────────────────
│      string      module root
│      error       no go.mod above the working dir
├─ example ───────────────────────────────────────
│      cwd internal/auth  →  repo root, not internal/
*/

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

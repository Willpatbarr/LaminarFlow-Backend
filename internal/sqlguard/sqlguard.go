/*
╔═ sqlguard.go ═════════════════════════════════════════════════════════════════════════
║  test support · table ownership
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      AssertOwned      func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/document  →  document · search_index
║      internal/auth      →  api_token
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

// Skipped by every guard: a schema constraint test proves a constraint by violating it,
// so naming the table is the technique. Shrinks as tables gain owners - api_token has
// one now, so schema_api_token_test.go belongs in internal/auth.
const schemaTestDir = "migrate"

/*
┌─ sqlguard ──────────────────────────────────────
│  fails tb on SQL naming a table outside its owner
├─ in ────────────────────────────────────────────
│      tb          testing.TB
│      ownerDir    string      skipped, with .git and migrate
│      tables      ...string   at least one
├─ example ───────────────────────────────────────
│      "auth", "api_token"  →  fails on internal/db hit
*/

func AssertOwned(tb testing.TB, ownerDir string, tables ...string) {
	tb.Helper()

	if len(tables) == 0 {
		tb.Fatal("AssertOwned called with no tables, so it would assert nothing")
	}

	root, err := moduleRoot()
	if err != nil {
		tb.Fatalf("locate module root: %v", err)
	}

	pattern := regexp.MustCompile(fmt.Sprintf(
		`(?i)\b(insert\s+into|update|delete\s+from|from|join)\s+"?(%s)\b`,
		strings.Join(tables, "|")))

	fset := token.NewFileSet()
	var offenders []string

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ownerDir || d.Name() == schemaTestDir {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}

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
			if pattern.MatchString(s) {
				offenders = append(offenders, fset.Position(lit.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		tb.Fatalf("walk: %v", err)
	}

	for _, o := range offenders {
		tb.Errorf("SQL against %s outside package %s: %s\n"+
			"  route it through that package's service - it is the only writer\n"+
			"  see docs/adr/0001-write-path-enforcement.md",
			strings.Join(tables, "/"), ownerDir, o)
	}
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

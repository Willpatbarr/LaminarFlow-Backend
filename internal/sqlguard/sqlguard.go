// Package sqlguard fails a test when SQL against an owned table appears
// outside the package that owns it.
//
// It is a package rather than a test copied per owner, for the reason
// internal/dbtest gives for itself: more than one package needs this now.
// internal/document owns document and search_index; internal/auth owns
// api_token. Two copies of an AST walk would be two things to keep in step,
// and the walk is the part that is easy to get subtly wrong - parsing with
// comments kept, or forgetting to skip the owner's own directory, both turn
// the guard into noise.
//
// Why a guard exists at all is docs/adr/0001-write-path-enforcement.md: Go's
// encapsulation is package-scoped, so an unexported pool stops a caller
// reaching through a service but does not stop a new package calling
// db.Connect and writing the table itself. There is no language feature that
// closes that, so this does.
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

// schemaTestDir is skipped by every guard, and the reason is narrow enough to
// state precisely.
//
// What a guard protects is the write path: production code reaching around the
// owning service, so the blob and its index drift, or so a token lookup
// escapes the one function a cache could later live in. A schema constraint
// test is not that. It proves a constraint fires by deliberately violating it,
// which means naming the table in an INSERT is the entire technique - there is
// no way to assert "api_token rejects a null account" without writing SQL that
// names api_token.
//
// internal/migrate is where those tests live for every table with no owning
// package, which E-LAM-0003 settled. Tables that do have an owner keep their
// schema tests with the owner instead - internal/document holds
// schema_document_test.go and schema_search_index_test.go for exactly that
// reason, which is why this exemption changes nothing for the document guard.
//
// The exemption should shrink, not grow. api_token now has an owner, so
// internal/migrate/schema_api_token_test.go belongs in internal/auth; moving
// it needs migratedPool and wantPgError ported alongside, which is its own
// piece of work rather than part of LAM-55.
const schemaTestDir = "migrate"

// AssertOwned fails tb if any package other than ownerDir issues SQL against
// one of tables.
//
// ownerDir is the directory name of the owning package - "document", "auth" -
// and is skipped entirely, along with .git and schemaTestDir. Everything else
// in the module is parsed.
//
// Only string literals are inspected, and files are parsed with comments
// discarded, so prose naming a table is never a failure. cmd/reindex relies on
// that: its package comment names search_index and stays green.
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

		// Mode 0 drops comments, so only real string literals are inspected.
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

// moduleRoot walks up from the working directory to the directory holding
// go.mod.
//
// Callers used to pass filepath.Join("..", "..") and that only worked from a
// package exactly two levels deep. Finding go.mod instead means a guard in a
// nested package does not silently walk a subtree of the module and pass.
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

package document

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// tableSQL matches a SQL statement naming either table this package owns.
var tableSQL = regexp.MustCompile(
	`(?i)\b(insert\s+into|update|delete\s+from|from|join)\s+"?(document|search_index)\b`)

// TestNoSQLOutsideThisPackage fails if any package other than this one issues
// SQL against document or search_index.
//
// Go's encapsulation is package-scoped: Service.pool being unexported stops a
// caller reaching through a Service, but nothing stops a new package calling
// db.Connect and writing the tables itself. That is the hole this closes.
//
// Only string literals are inspected, and files are parsed with comments
// discarded, so prose mentioning a table name is never a failure.
func TestNoSQLOutsideThisPackage(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || filepath.Base(path) == "document" {
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
			if tableSQL.MatchString(s) {
				offenders = append(offenders, fset.Position(lit.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, o := range offenders {
		t.Errorf("SQL against document/search_index outside package document: %s\n"+
			"  route it through document.Service so the blob and the index stay in sync\n"+
			"  see docs/adr/0001-write-path-enforcement.md", o)
	}
}

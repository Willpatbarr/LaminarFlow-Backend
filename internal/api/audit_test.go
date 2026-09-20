package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// The E-LAM-0004 close audit, run against the built OpenAPI document and the package's
// own source rather than against the tickets.
//
// Both guards here generalise a rule the epic stated per resource and enforced per
// resource. internal/api/ticket.go says "marking them individually rather than wrapping
// the group means a new one is unprotected only if someone omits the line - which the
// middleware test catches", and four resources each grew their own version of that
// test. A seventh resource gets no coverage from any of them.

// operations walks every registered operation once.
func operations(t *testing.T) map[string]*huma.Operation {
	t.Helper()

	doc := NewHumaAPI(http.NewServeMux(), nil, nil, nil, nil, nil, nil, nil).OpenAPI()

	out := map[string]*huma.Operation{}
	for path, item := range doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op == nil {
				continue
			}
			out[op.Method+" "+path] = op
		}
	}

	if len(out) == 0 {
		t.Fatal("no operations registered, so this guard proves nothing")
	}

	return out
}

// public is every operation that is allowed to have no caller, and why.
//
// An allowlist rather than a convention, so adding a public endpoint is a line in this
// file - a deliberate act with a reason attached - rather than a line someone forgot to
// write in a register function.
var public = map[string]string{
	"POST " + V1 + "/auth/login":  "there is no caller yet; that is what it is for",
	"POST " + V1 + "/auth/logout": "logging out with a dead cookie must clear it, not 401",
	"GET " + V1 + "/ping":         "a liveness probe carries no credential",
}

// Every operation in the API, not just the ones whose resource happened to grow a test.
func TestEveryOperationDeclaresItsAuth(t *testing.T) {
	for name, op := range operations(t) {
		required, _ := op.Metadata[RequireAuth].(bool)
		reason, isPublic := public[name]

		switch {
		case required && isPublic:
			t.Errorf("%s is marked RequireAuth but listed as public (%q) - one of the two is wrong",
				name, reason)
		case !required && !isPublic:
			t.Errorf("%s has no RequireAuth and is not in the public allowlist, so it is "+
				"unauthenticated by omission. Mark it, or add it to `public` with a reason.", name)
		}
	}

	// The allowlist must not outlive the endpoints it names, or it silently
	// pre-authorises a path someone adds later under the same name.
	all := operations(t)
	for name := range public {
		if _, ok := all[name]; !ok {
			t.Errorf("the public allowlist names %s, which is not registered", name)
		}
	}
}

// Every code a caller can branch on has to be findable. The epic added nine, across
// five files, and docs/api-errors.md is where a caller looks.
func TestEveryErrorCodeIsDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api-errors.md")
	if err != nil {
		t.Fatalf("read the error reference: %v", err)
	}

	codes := declaredCodes(t)
	if len(codes) < 5 {
		t.Fatalf("found %d code constants, which is too few to mean anything", len(codes))
	}

	for name, value := range codes {
		if !strings.Contains(string(doc), "`"+value+"`") {
			t.Errorf("%s = %q is not in docs/api-errors.md. A code is a public API "+
				"surface, and one a caller cannot find is one it parses `detail` for instead.",
				name, value)
		}
	}
}

// declaredCodes reads every Code* constant out of this package's own source.
//
// Parsed rather than listed, for the reason sqlguard is parsed rather than listed: a
// list is a second place to remember, and the failure it is guarding against is exactly
// somebody forgetting a place.
func declaredCodes(t *testing.T) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	codes := map[string]string{}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}

			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, "Code") || i >= len(spec.Values) {
					continue
				}

				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}

				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				codes[name.Name] = value
			}

			return true
		})
	}

	return codes
}

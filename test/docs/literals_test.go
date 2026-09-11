package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const repoRoot = "../.."

// stringLiterals returns, sorted and de-duplicated, the value of every string
// literal in the non-test Go files directly under dir (a repo-relative package
// directory) for which keep returns true. Comments are not literals, so a
// name that only appears in a comment never counts — which is what the drift
// tests want: they pin what the binaries read and print, not what they say.
func stringLiterals(t *testing.T, dir string, keep func(string) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(repoRoot, dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && keep(v) {
				seen[v] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// readDoc returns docs/<file> as a string.
func readDoc(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docsDir, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

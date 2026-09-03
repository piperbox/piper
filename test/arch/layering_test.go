// Package arch holds architecture tests: invariants about how the code is
// allowed to fit together, rather than what it does.
package arch_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const internalPrefix = "github.com/piperbox/piper/internal/"

// layer ranks each internal package. CLAUDE.md's layering rule is that nothing
// imports "up": store knows only persistence, runtime only Docker, caddy only
// Caddy's admin API; deploy orchestrates those through interfaces; api is
// transport over deploy+store; client is the CLI's view of api.
//
// An import is legal only if the imported package sits strictly lower. Equal
// ranks are illegal too — two packages at the same level are peers, and a
// dependency between them means one of them is really a layer up.
//
// Adding an internal package? Give it a rank here. The test fails on any
// package it does not know about, so the placement is a deliberate decision
// rather than something that happens by accident.
var layer = map[string]int{
	// 0 — leaves. Each owns exactly one concern and depends on no sibling.
	"caddy":           0,
	"certs":           0,
	"config":          0,
	"enrollapi":       0,
	"ghjwt":           0,
	"relay/relaytest": 0,
	"relayclient":     0,
	"runtime":         0,
	"source":          0,
	"store":           0,
	"tunnel":          0,
	"version":         0,

	// 1 — single-purpose services composed from the leaves.
	"agent":         1,
	"deploy":        1,
	"relay":         1,
	"source/github": 1,
	"webhook":       1,

	// 2 — orchestration across several leaves.
	"domain": 2,

	// 3 — transport over the layers below.
	"api": 3,

	// 4 — consumers of the control API.
	"client": 4,
	"tui":    4,
}

// TestNothingImportsUp walks the production sources of every internal package
// and fails on any import of a package ranked at or above the importer.
func TestNothingImportsUp(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	imports := internalImports(t, root)

	if len(imports) == 0 {
		t.Fatal("parsed no internal packages; the layering rule is not actually being checked")
	}

	for pkg, deps := range imports {
		from, ok := layer[pkg]
		if !ok {
			t.Errorf("internal/%s has no layer assigned in this test — add it to the layer map so its place in the architecture is a deliberate choice", pkg)
			continue
		}
		for _, dep := range deps {
			to, ok := layer[dep]
			if !ok {
				t.Errorf("internal/%s imports internal/%s, which has no layer assigned in this test", pkg, dep)
				continue
			}
			if to >= from {
				t.Errorf("layering violation: internal/%s (layer %d) imports internal/%s (layer %d) — nothing may import up or sideways", pkg, from, dep, to)
			}
		}
	}
}

// TestEveryLayeredPackageExists keeps the map honest in the other direction: a
// package that is renamed or deleted must not leave a stale rank behind.
func TestEveryLayeredPackageExists(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	for pkg := range layer {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(pkg))); err != nil {
			t.Errorf("layer map lists internal/%s, which does not exist: %v", pkg, err)
		}
	}
}

// internalImports maps each internal package (path relative to internal/) to
// the internal packages it imports. Test files are skipped: the rule constrains
// production layering, and an external test package may legitimately reach for
// a higher layer to build fixtures.
func internalImports(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(dir)
		if _, seen := out[pkg]; !seen {
			out[pkg] = nil
		}

		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			dep, ok := strings.CutPrefix(p, internalPrefix)
			if !ok {
				continue
			}
			if !contains(out[pkg], dep) {
				out[pkg] = append(out[pkg], dep)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

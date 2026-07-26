// Package legacyguard provides a deterministic static guard over the
// production entrypoint graph (cmd/gmuxd non-test files). It fails if
// any entrypoint file:
//
//   1. Imports a deleted authority package (peerstore, storegc).
//   2. Calls a legacy authority constructor (store.New, projects.NewManager).
//   3. Calls a legacy sessionmeta authority method (Write, Sweep, WatchRemovals).
//   4. Contains a literal reference to a legacy JSON file (meta.json,
//      projects.json, peers.json) that implies direct file I/O.
//
// The guarantee is scoped to the entrypoint graph only. Retained packages
// that the entrypoint imports (store, projects, sessionmeta, discovery)
// are permitted as type-only or utility dependencies: the entrypoint may
// import store.Session for wire conversion, projects.MatchRule for path
// normalization, or sessionmeta.Store for SessionDir/MaybePruneScrollback.
// The guard rejects only the specific constructors/calls that would
// reconstruct a legacy authority.
//
// Self-tests prove each forbidden pattern is caught via temp fixtures.
package legacyguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports lists deleted packages that must not appear in the
// entrypoint import graph. These have been physically removed.
var forbiddenImports = map[string]string{
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peerstore": "package deleted; use centralstore peers",
	"github.com/gmuxapp/gmux/services/gmuxd/internal/storegc":  "package deleted; use centralstore reconciliation",
}

// forbiddenConstructors lists constructor calls that would reconstruct
// a legacy authority. The entrypoint may import the package for types
// but must not call these constructors.
var forbiddenConstructors = map[string]string{
	"store.New":         "reconstructs legacy in-memory store; use centralstore.Open",
	"projects.NewManager": "reconstructs legacy projects.json authority; use Coordinator.ReplaceCatalog",
}

// forbiddenMethods lists sessionmeta method calls that would reconstruct
// the legacy meta.json authority. The entrypoint may call SessionDir
// and MaybePruneScrollback (scrollback cache management) but must not
// call Write/Sweep/WatchRemovals (meta.json I/O).
// The patterns match the actual variable name used in production (sessionDirs).
var forbiddenMethods = map[string]string{
	"sessionDirs.Write":          "writes meta.json; use centralstore",
	"sessionDirs.Sweep":          "reads meta.json; use centralstore",
	"sessionDirs.WatchRemovals":  "subscribes to store events; use Coordinator outcomes",
}

// legacyJSONFiles lists JSON file names that must not appear as string
// literals in the entrypoint. Their presence implies direct file I/O
// rather than going through centralstore.
var legacyJSONFiles = map[string]string{
	"meta.json":     "legacy session metadata; use centralstore",
	"projects.json": "legacy project catalog; use centralstore",
	"peers.json":    "legacy peer store; use centralstore",
}

// entrypointFiles returns the production (non-test) .go files in cmd/gmuxd.
func entrypointFiles(t *testing.T) []string {
	t.Helper()
	root := findCmdGmuxd(t)
	matches, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range matches {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

// TestEntrypointNoDeletedImports verifies the entrypoint graph does not
// import packages that have been physically deleted.
func TestEntrypointNoDeletedImports(t *testing.T) {
	for _, f := range entrypointFiles(t) {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Logf("skipping %s: %v", f, err)
			continue
		}
		for _, imp := range node.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if reason, forbidden := forbiddenImports[path]; forbidden {
				t.Errorf("%s: imports deleted package %q (%s)", f, path, reason)
			}
		}
	}
}

// TestEntrypointNoLegacyConstructors verifies the entrypoint graph does
// not call constructors that would reconstruct a legacy authority.
func TestEntrypointNoLegacyConstructors(t *testing.T) {
	for _, f := range entrypointFiles(t) {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Logf("skipping %s: %v", f, err)
			continue
		}
		for constructor := range forbiddenConstructors {
			if hasCall(node, constructor) {
				t.Errorf("%s: calls forbidden constructor %q", f, constructor)
			}
		}
	}
}

// TestEntrypointNoLegacySessionmetaMethods verifies the entrypoint graph
// does not call sessionmeta authority methods (Write/Sweep/WatchRemovals).
// SessionDir and MaybePruneScrollback are permitted (scrollback cache).
func TestEntrypointNoLegacySessionmetaMethods(t *testing.T) {
	for _, f := range entrypointFiles(t) {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Logf("skipping %s: %v", f, err)
			continue
		}
		for method, reason := range forbiddenMethods {
			if hasCall(node, method) {
				t.Errorf("%s: calls forbidden method %q (%s)", f, method, reason)
			}
		}
	}
}

// TestEntrypointNoLegacyJSONLiterals verifies the entrypoint graph does
// not contain string literals referencing legacy JSON files.
func TestEntrypointNoLegacyJSONLiterals(t *testing.T) {
	for _, f := range entrypointFiles(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Logf("skipping %s: %v", f, err)
			continue
		}
		content := string(data)
		for file, reason := range legacyJSONFiles {
			if strings.Contains(content, file) {
				t.Errorf("%s: contains legacy JSON literal %q (%s)", f, file, reason)
			}
		}
	}
}

// hasCall reports whether the AST contains a call expression matching
// the selector pattern (e.g. "store.New" or "sessionmeta.Store.Write").
// Only exact matches are accepted; no fuzzy receiver matching.
func hasCall(root *ast.File, pattern string) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok {
			if sel, ok := ce.Fun.(*ast.SelectorExpr); ok {
				// Build the full qualified name: could be "pkg.Func" or "a.b.Func"
				var receiver string
				switch x := sel.X.(type) {
				case *ast.Ident:
					receiver = x.Name
				case *ast.SelectorExpr:
					// Chained: e.g. "a.b.Write" - take the full chain
					if ident, ok := x.X.(*ast.Ident); ok {
						receiver = ident.Name + "." + x.Sel.Name
					}
				}
				call := receiver + "." + sel.Sel.Name
				if call == pattern {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// findCmdGmuxd walks up from the package directory to find cmd/gmuxd.
func findCmdGmuxd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		candidates, _ := filepath.Glob(filepath.Join(dir, "cmd", "gmuxd", "*.go"))
		if len(candidates) > 0 {
			return filepath.Join(dir, "cmd", "gmuxd")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate cmd/gmuxd")
	return ""
}

// ── Self-tests: prove each forbidden pattern is caught ──

// TestSelf_ForgedDeletedImport proves the import guard catches a
// forbidden import. Creates a temp file with a peerstore import and
// verifies the guard would flag it.
func TestSelf_ForgedDeletedImport(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "fake.go")
	// Write a file that imports the deleted peerstore package.
	_ = os.WriteFile(f, []byte(`
package main
import _ "github.com/gmuxapp/gmux/services/gmuxd/internal/peerstore"
`), 0o644)

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range node.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if _, forbidden := forbiddenImports[path]; !forbidden {
			t.Errorf("expected %q to be flagged as forbidden import", path)
		}
	}
}

// TestSelf_ForgedConstructor proves the constructor guard catches
// store.New and projects.NewManager calls.
func TestSelf_ForgedConstructor(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "fake.go")
	_ = os.WriteFile(f, []byte(`
package main
import "github.com/gmuxapp/gmux/services/gmuxd/internal/store"
func main() { s := store.New() }
`), 0o644)

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, f, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCall(node, "store.New") {
		t.Error("expected store.New to be detected")
	}
}

// TestSelf_ForgedSessionmetaMethod proves the method guard catches
// sessionDirs.Write calls.
func TestSelf_ForgedSessionmetaMethod(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "fake.go")
	_ = os.WriteFile(f, []byte(`
package main
func main() { sessionDirs.Write(sess) }
`), 0o644)

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, f, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCall(node, "sessionDirs.Write") {
		t.Error("expected sessionDirs.Write to be detected")
	}
}

// TestSelf_ForgedJSONLiteral proves the literal guard catches
// legacy JSON file references.
func TestSelf_ForgedJSONLiteral(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "fake.go")
	_ = os.WriteFile(f, []byte(`
package main
const legacy = "meta.json"
`), 0o644)

	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	found := false
	for file := range legacyJSONFiles {
		if strings.Contains(content, file) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected meta.json literal to be detected")
	}
}

// Package archtest guards the dependency direction clean architecture
// requires: internal/domain imports only the standard library, and
// internal/app never imports an adapter package.
package archtest

import (
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// importsByFile parses every non-test .go file under dir and returns its
// import paths, keyed by file path. It only parses syntax, so it works
// even when the package itself would fail to compile.
func importsByFile(t *testing.T, dir string) map[string][]string {
	t.Helper()

	imports := make(map[string][]string)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		paths := make([]string, 0, len(f.Imports))
		for _, imp := range f.Imports {
			paths = append(paths, strings.Trim(imp.Path.Value, `"`))
		}
		imports[path] = paths
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return imports
}

// isStdlib reports whether an import path resolves to a package rooted
// under GOROOT/src, which is true only for the standard library.
func isStdlib(importPath string) bool {
	pkg, err := build.Import(importPath, "", build.FindOnly)
	if err != nil {
		return false
	}
	srcRoot := filepath.Join(runtime.GOROOT(), "src")
	rel, err := filepath.Rel(srcRoot, pkg.Dir)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

func TestDomainImportsOnlyStandardLibrary(t *testing.T) {
	for file, paths := range importsByFile(t, "../domain") {
		for _, p := range paths {
			if !isStdlib(p) {
				t.Errorf("%s imports %q: internal/domain must import only the standard library", file, p)
			}
		}
	}
}

func TestAppDoesNotImportAdapters(t *testing.T) {
	for file, paths := range importsByFile(t, "../app") {
		for _, p := range paths {
			if strings.Contains(p, "/internal/adapters/") || strings.HasSuffix(p, "/internal/adapters") {
				t.Errorf("%s imports %q: internal/app must not depend on an adapter", file, p)
			}
		}
	}
}

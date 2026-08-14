package ormhealth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Release-critical, and a permanent architectural invariant: this package only
// reads.
//
// A health check that repaired what it found could not safely be given to a
// probe, and "the readiness endpoint migrated production" is a sentence that must
// never be true. The two checks below are structural rather than behavioural, so
// they hold for code nobody thought to write a behavioural test for.

// No statement this package can issue is a mutation.
func TestReadOnly_noMutatingSQL(t *testing.T) {
	// Every string literal in the package's non-test source, checked against
	// the statements that change something.
	mutating := regexp.MustCompile(`(?i)\b(insert\s+into|update\s+\w|delete\s+from|truncate|create\s+(table|index|extension|schema)|drop\s+|alter\s+(table|system|database)|analyze|vacuum|reindex|grant|revoke|set\s+\w+\s*=)\b`)

	for _, path := range sourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if m := mutating.FindString(lit.Value); m != "" {
				t.Errorf("%s: a string literal contains %q, which changes something:\n  %s",
					fset.Position(lit.Pos()), m, truncate(lit.Value))
			}
			return true
		})
	}
}

// No function this package calls is one that mutates.
//
// The SQL check above would miss a call into the migration engine, which issues
// its own statements. This catches that: applying migrations, generating them, or
// resizing a pool are all named, and none of them may be reached from here.
func TestReadOnly_noMutatingCalls(t *testing.T) {
	banned := map[string]string{
		"Migrate":     "applying migrations is a deliberate act, not a health check",
		"Up":          "applying migrations is a deliberate act, not a health check",
		"Apply":       "applying anything is a deliberate act, not a health check",
		"Generate":    "writing generated code is not a health check",
		"Exec":        "a health check reads; Query is enough for that",
		"SetMaxConns": "resizing a pool is tuning, which this package does not do",
		"Reset":       "resetting a pool is not reading it",
	}

	for _, path := range sourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, bad := banned[sel.Sel.Name]; bad {
				t.Errorf("%s: calls %s — %s", fset.Position(call.Pos()), sel.Sel.Name, why)
				_ = why
			}
			return true
		})
	}
}

// sourceFiles lists the package's own non-test Go files.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(".", name))
	}
	if len(out) == 0 {
		t.Fatal("no source files found; this guard would pass vacuously")
	}
	return out
}

func truncate(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "…"
}

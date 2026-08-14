package hexagonal_test

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The boundary, checked by a machine.
//
// Every architecture document claims its core does not depend on its
// infrastructure. The claim is worth exactly as much as the thing that checks
// it, and code review does not check it: an import added in a hurry looks like
// every other import. So this reads the actual import graph out of the Go
// toolchain and states the rule as a test.
//
// It is deliberately not a lint configuration. A rule that lives in CI's YAML
// is a rule that a developer discovers after pushing; a rule that lives in a
// test fails in the editor.

// pkg is the part of `go list -json` this test reads.
type pkg struct {
	ImportPath string
	Imports    []string
	Deps       []string
}

// listPackages asks the toolchain for the import graph of this module.
func listPackages(t *testing.T) map[string]pkg {
	t.Helper()
	out, err := exec.Command("go", "list", "-json", "./...").Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	pkgs := map[string]pkg{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		pkgs[p.ImportPath] = p
	}
	if len(pkgs) == 0 {
		t.Fatal("go list returned no packages")
	}
	return pkgs
}

func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

const mod = "example.com/hexagonal"

// Release-critical: the core depends on nothing outside itself.
//
// Not transitively either — Deps rather than Imports — because an import of a
// package that imports pgx is an import of pgx as far as compilation, binary
// size and coupling are concerned.
func TestBoundary_coreDependsOnNothingOutside(t *testing.T) {
	pkgs := listPackages(t)

	// Everything under core/ is the core.
	core := []string{
		mod + "/core/domain",
		mod + "/core/port",
		mod + "/core/app",
	}

	// The things the core may never reach, at any depth.
	forbidden := []string{
		"github.com/AlexAli29/orm",
		"github.com/jackc/pgx/v5",
		"net/http",
		"database/sql",
		mod + "/adapter/ormstore",
		mod + "/adapter/httpin",
	}

	for _, name := range core {
		p, ok := pkgs[name]
		if !ok {
			t.Fatalf("%s is not in the module; this test is checking a package that moved", name)
		}
		for _, dep := range p.Deps {
			// The core's own packages share the ORM's module path prefix, so
			// the rule has to exclude them explicitly rather than by prefix.
			if dep == mod || strings.HasPrefix(dep, mod+"/core/") {
				continue
			}
			for _, bad := range forbidden {
				if dep == bad || strings.HasPrefix(dep, bad+"/") {
					t.Errorf("%s depends on %s", name, dep)
				}
			}
		}
	}
}

// The innermost package imports the standard library and nothing else.
//
// This is stronger than the rule above and it is stated separately, because the
// domain is where the rule is most valuable and most easily eroded: one import
// of a UUID library or a validation framework, and the rules of the business
// have a dependency with a release schedule.
func TestBoundary_domainImportsOnlyTheStandardLibrary(t *testing.T) {
	pkgs := listPackages(t)
	p := pkgs[mod+"/core/domain"]
	for _, dep := range p.Deps {
		// A standard-library path has no dot in its first element.
		first, _, _ := strings.Cut(dep, "/")
		if strings.Contains(first, ".") {
			t.Errorf("the domain depends on %s", dep)
		}
	}
}

// Only the adapter knows the ORM exists.
//
// The count matters as much as the rule: if a second package starts importing
// the ORM, the thing the architecture was for — being able to say where the
// database is — has quietly stopped being true.
func TestBoundary_onlyTheAdapterKnowsTheORM(t *testing.T) {
	pkgs := listPackages(t)

	var importers []string
	for name, p := range pkgs {
		if strings.HasSuffix(name, ".test") {
			continue
		}
		// The negative control imports the ORM on purpose; see
		// TestBoundary_theCheckIsTransitive.
		if strings.HasPrefix(name, mod+"/control/") {
			continue
		}
		for _, imp := range p.Imports {
			if imp == "github.com/AlexAli29/orm" {
				importers = append(importers, name)
				break
			}
		}
	}
	slices.Sort(importers)

	want := []string{
		mod + "/adapter/ormstore",
		mod + "/cmd/server", // the composition root, which names every side once
	}
	if !slices.Equal(importers, want) {
		t.Errorf("the ORM is imported by\n  %s\nand should be imported by\n  %s",
			strings.Join(importers, "\n  "), strings.Join(want, "\n  "))
	}
}

// The driving adapter does not reach the store directly.
//
// An HTTP handler that imported the repository could run a query without a use
// case, and every rule the core holds would be optional from that moment on.
func TestBoundary_httpDoesNotReachTheStore(t *testing.T) {
	pkgs := listPackages(t)
	p := pkgs[mod+"/adapter/httpin"]
	for _, dep := range p.Deps {
		if dep == mod+"/adapter/ormstore" {
			t.Error("the HTTP adapter depends on the store adapter, so a handler can bypass the core")
		}
		if dep == "github.com/jackc/pgx/v5" || strings.HasPrefix(dep, "github.com/jackc/pgx/v5/") {
			t.Errorf("the HTTP adapter depends on %s", dep)
		}
	}
}

// Go itself refuses an import of the ORM's internals, because this module's
// path is outside the ORM's.
//
// A separate go.mod is not enough on its own, and finding that out is what this
// test is for. Go's internal rule is lexical: a package may import
// .../internal/x when its own import path shares the parent of that internal
// directory. A module called github.com/AlexAli29/orm/examples/hexagonal is
// inside github.com/AlexAli29/orm/, so the compiler would have allowed it —
// separate module or not. The module is called example.com/hexagonal for that
// reason, which also makes it an external consumer in the only sense that
// cannot be arranged: not by convention, by the compiler.
func TestBoundary_internalsAreUnreachable(t *testing.T) {
	pkgs := listPackages(t)
	for name, p := range pkgs {
		// Direct imports, not Deps: the ORM imports its own internal packages,
		// as it is entitled to, and those show up transitively in everything
		// that uses it. What Go forbids — and what this checks — is a package
		// outside the ORM's module naming one.
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, "github.com/AlexAli29/orm/internal/") ||
				strings.Contains(imp, "/gen/internal/") {
				t.Errorf("%s imports %s, which should not have compiled", name, imp)
			}
		}
	}
}

// The boundary check reads the whole dependency graph, not the direct imports.
//
// This is the property the other tests in this file rest on, and it is the one
// that would fail silently: a check written against direct imports passes on a
// clean tree exactly as a correct one does, and keeps passing when a violation
// arrives the way violations actually arrive — one hop away, through a helper
// somebody added.
//
// control/outer is that shape, deliberately. It imports the ORM through
// control/inner and not directly. If this test ever reports that outer's direct
// imports already contain the ORM, the control has stopped being a control.
func TestBoundary_theCheckIsTransitive(t *testing.T) {
	pkgs := listPackages(t)

	outer, ok := pkgs[mod+"/control/outer"]
	if !ok {
		t.Fatal("the negative control is missing; the transitive check is unverified")
	}

	const orm = "github.com/AlexAli29/orm"
	if slices.Contains(outer.Imports, orm) {
		t.Fatal("the control imports the ORM directly, so it no longer distinguishes the two checks")
	}
	if !slices.Contains(outer.Deps, orm) {
		t.Fatal("the control does not depend on the ORM at all; it cannot catch anything")
	}

	// And the control is not reachable from the application, which is what
	// keeps it a fixture rather than a violation of the rules it exists to
	// check.
	for _, name := range []string{
		mod + "/core/domain", mod + "/core/port", mod + "/core/app",
		mod + "/adapter/httpin", mod + "/adapter/ormstore", mod + "/cmd/server",
	} {
		for _, dep := range pkgs[name].Deps {
			if strings.HasPrefix(dep, mod+"/control/") {
				t.Errorf("%s depends on the control fixture %s", name, dep)
			}
		}
	}
}

package orm_test

import (
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The dependency boundary.
//
// Runtime packages are imported by every application that uses this ORM, so
// their dependency set is part of the product: a transitive dependency on a
// YAML parser or on the Go toolchain libraries would be inherited by every
// binary. The generator has no such constraint — it runs at build time, on a
// developer's machine or in CI.
//
// v1 is a single module on purpose. Splitting runtime and generator into two
// modules would make the boundary structural rather than tested, but it also
// makes every change a two-module release, which is the wrong trade before the
// API is stable. This test holds the line in the meantime.
var (
	// runtimePkgs are the packages an application links at run time: the
	// public API and the expression compiler underneath it.
	runtimePkgs = []string{
		"github.com/AlexAli29/orm",
		"github.com/AlexAli29/orm/internal/expr",
	}

	// runtimeAllowed lists the non-standard modules runtime packages may reach.
	//
	// pgx brings its own dependencies, and depending on pgx means depending on
	// them: there is no version of "stdlib and pgx only" that excludes what pgx
	// itself imports. They are listed rather than waved through so that a new
	// one appearing is a decision somebody makes here.
	runtimeAllowed = []string{
		"github.com/jackc/pgx/v5",
		"github.com/jackc/pgpassfile",
		"github.com/jackc/pgservicefile",
		"github.com/jackc/puddle/v2",
		"golang.org/x/crypto",
		"golang.org/x/text",
	}

	// generatorAllowed lists what the generator and CLI may additionally reach.
	generatorAllowed = []string{
		"github.com/jackc/pgx/v5",
		"golang.org/x/tools",
		"gopkg.in/yaml.v3",
		// Pulled in transitively by the three above.
		"github.com/jackc/pgpassfile",
		"github.com/jackc/pgservicefile",
		"github.com/jackc/puddle/v2",
		"golang.org/x/crypto",
		"golang.org/x/mod",
		"golang.org/x/sync",
		"golang.org/x/text",
	}
)

// listedPackage is the subset of `go list -json` this test reads.
type listedPackage struct {
	ImportPath string
	Standard   bool
	Module     *struct {
		Path string
	}
}

// deps returns the transitive dependencies of the named packages, excluding the
// standard library and this module's own packages.
func deps(t *testing.T, patterns ...string) []string {
	t.Helper()
	args := append([]string{"list", "-deps", "-json"}, patterns...)
	out, err := exec.CommandContext(t.Context(), "go", args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list: %v\n%s", err, exit.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var mods []string
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		if pkg.Standard || pkg.Module == nil {
			continue
		}
		if pkg.Module.Path == "github.com/AlexAli29/orm" {
			continue
		}
		if !slices.Contains(mods, pkg.Module.Path) {
			mods = append(mods, pkg.Module.Path)
		}
	}
	slices.Sort(mods)
	return mods
}

func TestDependencyBoundary_runtime(t *testing.T) {
	for _, mod := range deps(t, runtimePkgs...) {
		if !slices.Contains(runtimeAllowed, mod) {
			t.Errorf("the runtime package depends on %s; runtime code may depend only on the standard library and %s",
				mod, strings.Join(runtimeAllowed, ", "))
		}
	}
}

func TestDependencyBoundary_generator(t *testing.T) {
	for _, mod := range deps(t, "github.com/AlexAli29/orm/internal/gen/...", "github.com/AlexAli29/orm/cmd/...") {
		if !slices.Contains(generatorAllowed, mod) {
			t.Errorf("the generator depends on %s, which is not on the allow list; add it deliberately or drop the import", mod)
		}
	}
}

func TestDependencyBoundary_runtimeDoesNotImportTheGenerator(t *testing.T) {
	// internal/ holds both runtime internals and build-time helpers, so the
	// rule names what is forbidden rather than assuming a whole directory is.
	forbidden := []string{
		"github.com/AlexAli29/orm/internal/gen",
		"github.com/AlexAli29/orm/cmd",
		"github.com/AlexAli29/orm/internal/testdb",
	}
	for _, pkg := range runtimePkgs {
		out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			for _, bad := range forbidden {
				if strings.HasPrefix(line, bad) {
					t.Errorf("%s imports %s, which runs at build time and must not be linked into an application", pkg, line)
				}
			}
		}
	}
}

func TestDependencyBoundary_expressionCompilerIsStandardLibraryOnly(t *testing.T) {
	// The SQL compiler turns a tree into a string and a slice of arguments.
	// Nothing about that needs a driver, so it does not get one: keeping it
	// free of pgx is what lets the whole of SQL generation be tested, and
	// later reused, without a database anywhere near it.
	if mods := deps(t, "github.com/AlexAli29/orm/internal/expr"); len(mods) != 0 {
		t.Errorf("internal/expr depends on %s; it compiles SQL text and needs nothing but the standard library", strings.Join(mods, ", "))
	}
}

package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The manifest is the v1 compatibility mechanism, so it is itself tested.
//
// The tests below break the public API on purpose, regenerate, and require the
// manifest to differ. A manifest that did not notice would be worse than none:
// it would be a green check over a broken contract, which is the failure mode
// this replaced. Export counting could not see any of these — every one of them
// either keeps the count identical or changes it in a way a threshold would
// wave through.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// generate runs the tool over a tree and returns the manifest.
func generate(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("go", "run", "./internal/tools/apimanifest", "-dir", dir, "./...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok {
			// A module whose own floor is above the running toolchain cannot be
			// loaded, and saying "the public API differs" about it would be a
			// lie: nothing was compared. ormtest/postgres declares 1.25 because
			// Testcontainers does, so running the suite under the project's
			// declared 1.24 floor lands here.
			//
			// This skips rather than fails, and only for that one reason. Every
			// other failure is still a failure — a gate that swallowed them all
			// would be a gate that never fired.
			if bytes.Contains(ee.Stderr, []byte("go.mod requires go >=")) {
				t.Skipf("this toolchain cannot load %s, so its manifest was not compared:\n%s",
					dir, ee.Stderr)
			}
			t.Fatalf("apimanifest: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("apimanifest: %v", err)
	}
	return string(out)
}

func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// tree copies a small module that exercises every construct the manifest is
// supposed to record, so a mutation can be applied to it without touching the
// repository.
func tree(t *testing.T, api string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/subject\n\ngo 1.24\n")
	write(t, filepath.Join(dir, "api.go"), api)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// base is the subject API. Each mutation below is a single edit to it.
const base = `package subject

// Store is a thing.
type Store struct {
	// ID identifies it.
	ID int64
	// Name names it.
	Name string
}

// Get gets one.
func (s *Store) Get(id int64) (string, error) { return "", nil }

// Put puts one.
func (s *Store) Put(name string) error { return nil }

// Number is a constraint.
type Number interface{ ~int64 | ~float64 }

// Box holds one.
type Box[T Number] struct{ V T }

// Unbox returns it.
func (b Box[T]) Unbox() T { return b.V }

// Reader is implemented by callers.
type Reader interface {
	Read() ([]byte, error)
}

// Open opens.
func Open(name string) (*Store, error) { return nil, nil }
`

func TestManifest_catchesBreakingChanges(t *testing.T) {
	before := generate(t, tree(t, base))

	for _, c := range []struct {
		what string
		from string
		to   string
	}{
		{"a removed exported function", "func Open(name string) (*Store, error) { return nil, nil }", ""},
		{"a renamed exported method", "func (s *Store) Put(name string) error", "func (s *Store) Write(name string) error"},
		{"a widened integer field", "\tID int64", "\tID int"},
		{"a changed parameter type", "func (s *Store) Get(id int64)", "func (s *Store) Get(id int32)"},
		{"a changed result type", "func Open(name string) (*Store, error) { return nil, nil }", "func Open(name string) (Store, error) { return Store{}, nil }"},
		{"a pointer receiver becoming a value receiver", "func (s *Store) Get(", "func (s Store) Get("},
		{"a tightened generic constraint", "type Number interface{ ~int64 | ~float64 }", "type Number interface{ ~int64 }"},
		{"a removed struct field", "\t// Name names it.\n\tName string\n", ""},
		{"a method added to a consumer-implemented interface", "\tRead() ([]byte, error)\n", "\tRead() ([]byte, error)\n\tClose() error\n"},
		{"an exported field becoming unexported", "\tName string", "\tname string"},
		{"a new exported function", "// Open opens.", "// Close closes.\nfunc Close() error { return nil }\n\n// Open opens."},
	} {
		t.Run(c.what, func(t *testing.T) {
			mutated := strings.Replace(base, c.from, c.to, 1)
			if mutated == base {
				t.Fatalf("the mutation did not apply; the anchor %q is not in the subject", c.from)
			}
			after := generate(t, tree(t, mutated))
			if after == before {
				t.Errorf("%s produced an identical manifest, so CI would not have seen it", c.what)
			}
		})
	}
}

// The manifest is stable: the same input produces the same output, and moving a
// declaration between files changes nothing.
func TestManifest_isDeterministic(t *testing.T) {
	dir := tree(t, base)
	first := generate(t, dir)
	for range 3 {
		if again := generate(t, dir); again != first {
			t.Fatal("two runs over one tree produced different manifests")
		}
	}

	// The same API split across two files, and the declarations reordered.
	split := t.TempDir()
	write(t, filepath.Join(split, "go.mod"), "module example.com/subject\n\ngo 1.24\n")
	idx := strings.Index(base, "// Number is a constraint.")
	write(t, filepath.Join(split, "zz_first.go"), "package subject\n\n"+base[idx:])
	write(t, filepath.Join(split, "aa_second.go"), base[:idx])

	if got := generate(t, split); got != first {
		t.Errorf("splitting the API across files changed the manifest:\n%s", diff(first, got))
	}
}

// The manifest contains nothing machine-specific: it is committed, and two
// developers must produce the same bytes.
func TestManifest_hasNoLocalPaths(t *testing.T) {
	dir := tree(t, base)
	got := generate(t, dir)
	for _, bad := range []string{dir, os.TempDir(), "/home/", "/Users/", "0x"} {
		if strings.Contains(got, bad) {
			t.Errorf("the manifest contains %q, which differs between machines", bad)
		}
	}
	// And the same tree in a different directory renders identically.
	if other := generate(t, tree(t, base)); other != got {
		t.Error("the manifest depends on where the module happens to sit")
	}
}

func diff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "-"+x, "+"+y)
		}
	}
	return strings.Join(out, "\n")
}

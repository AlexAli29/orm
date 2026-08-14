package ormextdemo_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Release-critical for the SDK's claim: this package reaches nothing internal.
//
// An extension that could import internal/expr would be able to build nodes the
// compiler never checked, forge a source it does not own, and bypass scope and
// nullability entirely. The whole point of the SDK is that a third party cannot
// do any of that — so the proof has to be that this fixture, which is a third
// party, does not.
//
// It is a static check rather than a behavioural one because the property is
// about what can be reached, not about what happens to be called today.
func TestFixture_importsNothingInternal(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		checked++
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "/internal/") || strings.HasSuffix(path, "/internal") {
				t.Errorf("%s imports %s; an extension must reach nothing internal",
					fset.Position(imp.Pos()), path)
			}
			// The public root and its public subpackages are the whole surface
			// an extension is entitled to.
			if strings.HasPrefix(path, "github.com/AlexAli29/orm") {
				switch path {
				case "github.com/AlexAli29/orm",
					"github.com/AlexAli29/orm/observe",
					"github.com/AlexAli29/orm/plan",
					"github.com/AlexAli29/orm/diagnostics":
				default:
					t.Errorf("%s imports %s, which is not part of the extension SDK",
						fset.Position(imp.Pos()), path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go files were checked; this guard would pass vacuously")
	}
}

// The module path is what makes Go itself refuse an internal import, and it is
// worth pinning.
//
// Go's internal rule works on import paths rather than on modules. A module
// named github.com/AlexAli29/orm/ormextdemo would sit lexically inside the ORM,
// and the compiler would allow it to import internal/expr — the separate go.mod
// would not help. Naming this fixture as a third party is what turns the
// guarantee into one the compiler enforces, so renaming it back would quietly
// remove that.
func TestFixture_hasAThirdPartyModulePath(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := string(data)
	if !strings.Contains(mod, "module example.com/ormextdemo") {
		t.Errorf("this fixture is meant to be a third party; go.mod says:\n%s", mod)
	}
	if strings.Contains(mod, "module github.com/AlexAli29/orm/") {
		t.Error("the module path sits inside the ORM, so Go would permit internal imports")
	}
}

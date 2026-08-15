// Package doccheck holds no code. It holds a test that reads the documentation
// site's Go snippets and checks every symbol they name against the packages
// that would have to provide it.
//
// Why this exists: the site's prose was written partly from memory of the API
// rather than from the API, and it accumulated calls that do not exist —
// orm.Add, orm.Lit, EqCol, GtCol, EqOf, AddOf, orm.Assign, Tags.Contains,
// Meta.HasKey, Users.Table(). Every one of them would have failed to compile for
// the first reader who tried it, and none of them failed anything here, because
// documentation is not built.
//
// Compiling the snippets outright is the stronger check and needs a fixture
// package declaring every entity the docs mention — Users, Orders, Places,
// Bookings, Articles and a dozen more — which is a large fixture to keep in step
// with prose that is meant to be free to invent an example. So this checks the
// thing that was actually wrong: whether the symbols exist.
package doccheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// contentDir is the site's content, relative to this package.
const contentDir = "../../../website/src/content"

// packages maps the qualifier a snippet writes to the directory providing it.
var packages = map[string]string{
	"orm":       "../../..",
	"postgis":   "../../../postgis",
	"ormtest":   "../../../ormtest",
	"ormslog":   "../../../ormslog",
	"observe":   "../../../observe",
	"ormhealth": "../../../ormhealth",
	"ormotel":   "../../../ormotel",
}

// exportedOf collects the exported package-level names a directory declares.
//
// It reads the source rather than a manifest so that a package without one is
// still checkable, and so that the answer cannot lag behind the code the way a
// generated file can.
// genericTypes records the exported types that take type parameters, which is
// what makes them impossible to call like a function without them.
var genericTypes = map[string]bool{}

func exportedOf(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.IsExported() {
						out[d.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								out[s.Name.Name] = true
								if s.TypeParams != nil {
									genericTypes[s.Name.Name] = true
								}
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									out[n.Name] = true
								}
							}
						}
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no exported names, which cannot be right", dir)
	}
	return out
}

// methodsOf collects every exported method name declared in a directory, across
// all receivers.
//
// Receiver-blind on purpose. Knowing that Contains exists on RangeCol and not on
// an array column needs a type checker and a fixture; knowing that HasKey exists
// on nothing at all is enough to catch what went wrong here, and it costs one
// pass over the AST.
func methodsOf(t *testing.T, dirs ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv == nil || !fn.Name.IsExported() {
						continue
					}
					out[fn.Name.Name] = true
				}
				// Interface methods count too: a docs snippet may call one.
				ast.Inspect(file, func(n ast.Node) bool {
					it, ok := n.(*ast.InterfaceType)
					if !ok {
						return true
					}
					for _, m := range it.Methods.List {
						for _, name := range m.Names {
							if name.IsExported() {
								out[name.Name] = true
							}
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// snippet is one fenced Go block.
type snippet struct {
	file string
	line int
	body string
}

var fence = regexp.MustCompile("(?s)```go\\n(.*?)```")

func goSnippets(t *testing.T) []snippet {
	t.Helper()
	var out []snippet
	err := filepath.WalkDir(contentDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, m := range fence.FindAllStringSubmatchIndex(src, -1) {
			out = append(out, snippet{
				file: path,
				line: 1 + strings.Count(src[:m[0]], "\n"),
				body: src[m[2]:m[3]],
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the content: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("found %d Go snippets, which is fewer than the site has; the walk "+
			"is not seeing the content", len(out))
	}
	return out
}

// qualified matches pkg.Symbol, and call matches .Method(.
var (
	qualified = regexp.MustCompile(`\b(orm|postgis|ormtest|ormslog|observe|ormhealth|ormotel)\.([A-Z]\w*)`)
	call      = regexp.MustCompile(`\.([A-Z]\w*)\(`)
)

// Every qualified symbol a snippet names has to exist in the package it names.
//
// This is the check that would have caught orm.Add, orm.Lit and orm.Assign the
// day each was written.
func TestDocs_qualifiedSymbolsExist(t *testing.T) {
	known := map[string]map[string]bool{}
	for qual, dir := range packages {
		known[qual] = exportedOf(t, dir)
	}

	type miss struct{ file, sym string }
	var misses []miss
	seen := map[string]bool{}

	for _, s := range goSnippets(t) {
		for _, m := range qualified.FindAllStringSubmatch(s.body, -1) {
			qual, sym := m[1], m[2]
			if known[qual][sym] {
				continue
			}
			key := qual + "." + sym + s.file
			if seen[key] {
				continue
			}
			seen[key] = true
			misses = append(misses, miss{s.file, qual + "." + sym})
		}
	}

	sort.Slice(misses, func(i, j int) bool { return misses[i].sym < misses[j].sym })
	for _, m := range misses {
		t.Errorf("%s names %s, which no package exports", m.file, m.sym)
	}
}

// Every method a snippet calls has to be a method somewhere.
//
// Receiver-blind, so it does not catch a real method called on the wrong type.
// It does catch a method called on nothing, which is what Meta.HasKey and
// Users.Table() were.
func TestDocs_methodsExist(t *testing.T) {
	dirs := make([]string, 0, len(packages))
	for _, d := range packages {
		dirs = append(dirs, d)
	}
	known := methodsOf(t, dirs...)

	// Names that belong to the standard library, to pgx, or to an entity the
	// prose invented. A docs example may call Close, Scan, Err or a method of
	// its own example type, and none of those are this package's to know about.
	allowed := map[string]bool{
		"Close": true, "Scan": true, "Err": true, "Next": true, "Exec": true,
		"Query": true, "QueryRow": true, "Background": true, "Now": true,
		"Sub": true, "Add": true, "Before": true, "After": true, "IsZero": true,
		"String": true, "Error": true, "Unwrap": true, "Is": true, "As": true,
		"New": true, "NewString": true, "Getenv": true, "Fatal": true,
		"Fatalf": true, "Errorf": true, "Printf": true, "Println": true,
		"Context": true, "Helper": true, "TempDir": true, "Cleanup": true,
		"Run": true, "Handle": true, "ListenAndServe": true, "Shutdown": true,
		"Chunk": true, "Contains": true, "Replace": true, "Split": true,
		"ByID": true, "Save": true, "Search": true, "Items": true, "Walk": true,
		"Duration": true, "Bounds": true, "ParseConfig": true,
		"WithTimeout": true,
		// Generated code and third-party packages the examples use. A generated
		// DB has Tx and TxOptions; pgxpool has NewWithConfig; a docs example may
		// name a router or a server of its own.
		"Tx": true, "TxOptions": true, "NewWithConfig": true, "Connect": true,
		"Exit": true, "Routes": true, "Handler": true, "WithImage": true,
		"WithErrorMessages": true, "WithMigrationState": true, "Rows": true,
		"Text": true, "WithSchemaCheck": true, "Quick": true, "Deep": true,
		"Done": true, "HandleFunc": true, "NewTicker": true, "Stop": true,
		"WriteHeader": true, "Observe": true,
	}

	var misses []string
	seen := map[string]bool{}
	for _, s := range goSnippets(t) {
		// Qualified calls are the other test's business, and orm.ArgOf( matches
		// the method pattern as readily as x.ArgOf( does. They are blanked
		// first so that only real selector calls remain.
		body := qualified.ReplaceAllString(s.body, "_")
		for _, m := range call.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if known[name] || allowed[name] || seen[name] {
				continue
			}
			seen[name] = true
			misses = append(misses, name+"  ("+s.file+")")
		}
	}
	sort.Strings(misses)
	for _, m := range misses {
		t.Errorf("snippets call .%s, which is a method of no exported type", m)
	}
}

// The two languages ship the same code.
//
// Prose is translated; code is not. A snippet that drifted in one language is a
// snippet somebody fixed once, and this is the cheapest way to notice.
func TestDocs_codeIsIdenticalInBothLanguages(t *testing.T) {
	collect := func(locale string) map[string][]string {
		out := map[string][]string{}
		for _, s := range goSnippets(t) {
			if !strings.Contains(s.file, string(filepath.Separator)+locale+string(filepath.Separator)) {
				continue
			}
			rel := s.file[strings.Index(s.file, locale+string(filepath.Separator))+len(locale)+1:]
			out[rel] = append(out[rel], strings.TrimSpace(s.body))
		}
		return out
	}
	en, ru := collect("en"), collect("ru")

	for page, blocks := range en {
		other, ok := ru[page]
		if !ok {
			t.Errorf("%s exists in English and not in Russian", page)
			continue
		}
		if len(blocks) != len(other) {
			t.Errorf("%s has %d Go blocks in English and %d in Russian",
				page, len(blocks), len(other))
			continue
		}
		for i := range blocks {
			// Comments inside code are translated; the code around them is not,
			// so the comparison drops comment lines.
			if stripComments(blocks[i]) != stripComments(other[i]) {
				t.Errorf("%s block %d differs between languages once comments are "+
					"removed:\n--- en\n%s\n--- ru\n%s", page, i+1, blocks[i], other[i])
			}
		}
	}
	for page := range ru {
		if _, ok := en[page]; !ok {
			t.Errorf("%s exists in Russian and not in English", page)
		}
	}
}

func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//") {
			continue
		}
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimRight(line[:i], " \t")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(strings.TrimSpace(line))
		b.WriteString("\n")
	}
	return b.String()
}

// A generic type is not a constructor.
//
// The symbol check passes orm.Returning(...) because Returning exists — as a
// type. Written as a call it cannot compile: a generic type needs its type
// arguments, and there is no function of that name to fall back on. That is
// exactly how orm.Returning(Summaries) survived a check designed to catch
// invented API.
func TestDocs_genericTypesAreNotCalled(t *testing.T) {
	for qual, dir := range packages {
		_ = exportedOf(t, dir) // populates genericTypes
		_ = qual
	}
	funcs := map[string]bool{}
	for _, dir := range packages {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				for _, decl := range file.Decls {
					if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.IsExported() {
						funcs[fn.Name.Name] = true
					}
				}
			}
		}
	}

	// pkg.Symbol( — a call, rather than a mention.
	called := regexp.MustCompile(`\b(orm|postgis|ormtest|ormslog|observe|ormhealth|ormotel)\.([A-Z]\w*)\(`)
	seen := map[string]bool{}
	for _, s := range goSnippets(t) {
		for _, m := range called.FindAllStringSubmatch(s.body, -1) {
			sym := m[2]
			if funcs[sym] || !genericTypes[sym] || seen[sym] {
				continue
			}
			seen[sym] = true
			t.Errorf("%s calls %s.%s, which is a generic type and not a function",
				s.file, m[1], sym)
		}
	}
}

// The call-shape check.
//
// The three tests above ask whether a name exists. None of them asks whether
// the call around it is well formed, and that gap is not theoretical: the
// documentation taught Update(ctx) for a method taking none, Set(column, value)
// for one taking an assignment, and orm.Window[Ride]() for a function with no
// type parameters at all. Every one of those named something real and none of
// them would compile.
//
// So this reads the signatures out of the packages and compares them with the
// calls the snippets actually write. It is deliberately one-sided about type
// arguments: supplying fewer than declared is ordinary inference and always
// allowed, while supplying more than exist cannot be anything but a mistake.
// Value arguments are exact, except for a variadic tail.
//
// Only package-level functions are checked. A method's signature depends on its
// receiver, which a snippet does not name and this cannot resolve — so the
// receiver-blind arity of a method would accept whatever the loosest overload
// takes, which is worse than not asking.

type sig struct {
	typeParams int
	params     int
	variadic   bool
}

func signaturesOf(t *testing.T, dir string) map[string]sig {
	t.Helper()
	out := map[string]sig{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	count := func(fl *ast.FieldList) int {
		if fl == nil {
			return 0
		}
		n := 0
		for _, f := range fl.List {
			if len(f.Names) == 0 {
				n++ // an unnamed parameter still occupies a position
				continue
			}
			n += len(f.Names)
		}
		return n
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !fn.Name.IsExported() {
					continue
				}
				s := sig{typeParams: count(fn.Type.TypeParams), params: count(fn.Type.Params)}
				if n := fn.Type.Params; n != nil && len(n.List) > 0 {
					if _, ok := n.List[len(n.List)-1].Type.(*ast.Ellipsis); ok {
						s.variadic = true
					}
				}
				out[fn.Name.Name] = s
			}
		}
	}
	return out
}

// parseSnippet gets an AST out of a fragment. Most snippets are statements, some
// are declarations, and some are neither — an elided chain, a line with a
// comment standing in for an argument. The unparseable ones are counted rather
// than failed on, because a cookbook that cannot show a fragment is a worse
// cookbook.
func parseSnippet(body string) (*ast.File, bool) {
	fset := token.NewFileSet()
	if f, err := parser.ParseFile(fset, "", "package p\n"+body, 0); err == nil {
		return f, true
	}
	wrapped := "package p\nfunc _() {\n" + body + "\n}\n"
	if f, err := parser.ParseFile(fset, "", wrapped, 0); err == nil {
		return f, true
	}
	return nil, false
}

func TestDocs_callsMatchTheirSignatures(t *testing.T) {
	sigs := map[string]map[string]sig{}
	for qualifier, dir := range packages {
		sigs[qualifier] = signaturesOf(t, dir)
	}

	var parsed, skipped int
	for _, sn := range goSnippets(t) {
		file, ok := parseSnippet(sn.body)
		if !ok {
			skipped++
			continue
		}
		parsed++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Peel the type arguments, which the parser reports as an index.
			fun, typeArgs := call.Fun, 0
			switch idx := fun.(type) {
			case *ast.IndexExpr:
				fun, typeArgs = idx.X, 1
			case *ast.IndexListExpr:
				fun, typeArgs = idx.X, len(idx.Indices)
			}

			selector, ok := fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			known, ok := sigs[pkg.Name]
			if !ok {
				return true
			}
			want, ok := known[selector.Sel.Name]
			if !ok {
				return true // an unknown name is the other tests' business
			}

			where := fmt.Sprintf("%s:%d", sn.file, sn.line)
			name := pkg.Name + "." + selector.Sel.Name

			if typeArgs > want.typeParams {
				t.Errorf("%s calls %s with %d type argument(s); it declares %d",
					where, name, typeArgs, want.typeParams)
			}

			// A call spreading a slice says nothing about how many the callee wants.
			if call.Ellipsis.IsValid() {
				return true
			}
			switch {
			case want.variadic && len(call.Args) < want.params-1:
				t.Errorf("%s calls %s with %d argument(s); it wants at least %d",
					where, name, len(call.Args), want.params-1)
			case !want.variadic && len(call.Args) != want.params:
				t.Errorf("%s calls %s with %d argument(s); it takes %d",
					where, name, len(call.Args), want.params)
			}
			return true
		})
	}

	t.Logf("checked calls in %d snippets; %d were fragments that do not parse", parsed, skipped)
}

// The structure check.
//
// The parity test above compares Go blocks and says nothing about anything else,
// which is how a yaml example came to sit under the wrong heading in Russian:
// the Go blocks still matched one for one, and every block after the untracked
// yaml one had quietly shifted up by a section.
//
// Headings are translated, so their text cannot be compared. Their shape can.
// A page and its translation must have the same sequence of heading levels and
// the same sequence of fence languages — which is enough to catch a section that
// was dropped, doubled, or filled with the wrong example.

var (
	heading  = regexp.MustCompile(`(?m)^(#{2,6}) `)
	anyFence = regexp.MustCompile("(?m)^```([a-z]*)")
)

func TestDocs_translationsHaveTheSameStructure(t *testing.T) {
	shapeOf := func(src string) (levels []int, langs []string) {
		for _, m := range heading.FindAllStringSubmatch(src, -1) {
			levels = append(levels, len(m[1]))
		}
		for _, m := range anyFence.FindAllStringSubmatch(src, -1) {
			langs = append(langs, m[1])
		}
		return levels, langs
	}

	en := filepath.Join(contentDir, "en")
	err := filepath.WalkDir(en, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		rel, err := filepath.Rel(en, path)
		if err != nil {
			return err
		}
		other := filepath.Join(contentDir, "ru", rel)

		a, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(other)
		if err != nil {
			t.Errorf("%s has no Russian counterpart", rel)
			return nil
		}

		enLevels, enLangs := shapeOf(string(a))
		ruLevels, ruLangs := shapeOf(string(b))

		// A fence sequence is opening and closing markers interleaved; comparing
		// the whole sequence covers both, and the language is only on the opener.
		if len(enLangs) != len(ruLangs) {
			t.Errorf("%s has %d fences in English and %d in Russian", rel, len(enLangs), len(ruLangs))
		} else {
			for i := range enLangs {
				if enLangs[i] != ruLangs[i] {
					t.Errorf("%s: fence %d is %q in English and %q in Russian",
						rel, i, enLangs[i], ruLangs[i])
					break
				}
			}
		}

		if len(enLevels) != len(ruLevels) {
			t.Errorf("%s has %d headings in English and %d in Russian — a section was "+
				"dropped or added in translation", rel, len(enLevels), len(ruLevels))
			return nil
		}
		for i := range enLevels {
			if enLevels[i] != ruLevels[i] {
				t.Errorf("%s: heading %d is level %d in English and level %d in Russian",
					rel, i, enLevels[i], ruLevels[i])
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the English content: %v", err)
	}
}

// Command godoclint reports exported symbols that carry no useful
// documentation.
//
// "Useful" is doing work in that sentence. A comment that restates the
// identifier — "Foo is Foo", "Get gets" — passes a presence check and tells a
// reader nothing, so this rejects it: a doc comment must say something the name
// does not already say. That is the difference between documentation and a
// lint-satisfying ritual.
//
// Generated files are skipped. Their documentation is written once, in the
// generator, and checking every emitted copy of it would measure the same
// sentence a thousand times.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

func main() {
	dir := flag.String("dir", ".", "module directory")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	missing, checked, err := run(*dir, patterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "godoclint: %v\n", err)
		os.Exit(2)
	}
	for _, m := range missing {
		fmt.Println(m)
	}
	fmt.Fprintf(os.Stderr, "godoclint: %d of %d exported symbols documented (%d undocumented)\n",
		checked-len(missing), checked, len(missing))
	if len(missing) > 0 {
		os.Exit(1)
	}
}

func run(dir string, patterns []string) (missing []string, checked int, err error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedFiles | packages.NeedTypesInfo,
		Dir:  dir,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, 0, err
	}

	for _, p := range pkgs {
		if p.Name == "main" || skip(p.PkgPath) {
			continue
		}
		var hasPackageDoc bool
		for i, f := range p.Syntax {
			if i < len(p.CompiledGoFiles) && generated(f) {
				continue
			}
			if f.Doc != nil && strings.TrimSpace(f.Doc.Text()) != "" {
				hasPackageDoc = true
			}
			for _, decl := range f.Decls {
				m, n := check(p.Fset, p.PkgPath, f, decl)
				missing = append(missing, m...)
				checked += n
			}
		}
		checked++
		if !hasPackageDoc {
			missing = append(missing, fmt.Sprintf("%s: the package itself has no doc comment", p.PkgPath))
		}
	}
	slices.Sort(missing)
	return missing, checked, nil
}

func skip(path string) bool {
	return strings.Contains(path, "/internal/") || strings.HasSuffix(path, "/internal") ||
		strings.Contains(path, "/testdata/") || strings.Contains(path, "/examples/")
}

// generated reports whether a file carries the standard generated-code marker.
func generated(f *ast.File) bool {
	for _, g := range f.Comments {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "// Code generated ") && strings.HasSuffix(c.Text, " DO NOT EDIT.") {
				return true
			}
		}
	}
	return false
}

func check(fset *token.FileSet, pkg string, f *ast.File, decl ast.Decl) (missing []string, checked int) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !d.Name.IsExported() {
			return nil, 0
		}
		if d.Recv != nil && !receiverExported(d.Recv) {
			return nil, 0
		}
		checked++
		name := d.Name.Name
		if d.Recv != nil {
			name = receiverName(d.Recv) + "." + name
		}
		if !documented(d.Doc, d.Name.Name) {
			missing = append(missing, fmt.Sprintf("%s: %s", pkg, name))
		}

	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if !s.Name.IsExported() {
					continue
				}
				checked++
				if !documented(s.Doc, s.Name.Name) && !documented(d.Doc, s.Name.Name) {
					missing = append(missing, fmt.Sprintf("%s: %s", pkg, s.Name.Name))
				}
				checked += structFields(fset, pkg, s, &missing)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if !n.IsExported() {
						continue
					}
					checked++
					// A grouped const block documented once at the top is the
					// idiomatic form and counts for its members.
					if !documented(s.Doc, n.Name) && !documented(d.Doc, n.Name) {
						missing = append(missing, fmt.Sprintf("%s: %s", pkg, n.Name))
					}
				}
			}
		}
	}
	return missing, checked
}

// structFields checks exported fields of an exported struct.
//
// A field is part of the API as much as a method: a caller reads it, sets it in
// a literal, and needs to know what it means when it is zero.
func structFields(fset *token.FileSet, pkg string, s *ast.TypeSpec, missing *[]string) int {
	st, ok := s.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return 0
	}
	// A comment above a run of adjacent fields documents all of them. That is
	// how Go is written and how godoc renders it, so requiring one comment per
	// field would not improve the documentation — it would produce a paragraph
	// saying the same thing six times. A blank line ends the run.
	n := 0
	covered := false
	prevEnd := 0
	for _, f := range st.Fields.List {
		startLine := fset.Position(f.Pos()).Line
		if f.Doc != nil {
			startLine = fset.Position(f.Doc.Pos()).Line
		}
		adjacent := prevEnd > 0 && startLine <= prevEnd+1
		switch {
		case f.Doc != nil || f.Comment != nil:
			covered = documented(f.Doc, fieldNames(f)) || documented(f.Comment, fieldNames(f))
		case !adjacent:
			covered = false
		}
		prevEnd = fset.Position(f.End()).Line

		for _, name := range f.Names {
			if !name.IsExported() {
				continue
			}
			n++
			if !covered {
				*missing = append(*missing, fmt.Sprintf("%s: %s.%s", pkg, s.Name.Name, name.Name))
			}
		}
	}
	return n
}

func receiverExported(recv *ast.FieldList) bool {
	name := receiverName(recv)
	return name != "" && ast.IsExported(strings.TrimPrefix(name, "*"))
}

func receiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	switch t := recv.List[0].Type.(type) {
	case *ast.StarExpr:
		return "*" + typeName(t.X)
	default:
		return typeName(t)
	}
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	}
	return ""
}

// documented reports whether a comment says anything beyond the name.
func documented(doc *ast.CommentGroup, name string) bool {
	if doc == nil {
		return false
	}
	text := strings.TrimSpace(doc.Text())
	if text == "" {
		return false
	}
	// A deprecation notice is documentation: it says what to use instead.
	if strings.Contains(text, "Deprecated:") {
		return true
	}
	// Strip the conventional "Name ..." opening and see what is left. A comment
	// whose whole content is the name and a copula says nothing.
	rest := strings.TrimSpace(strings.TrimPrefix(text, name))
	for _, filler := range []string{"is", "are", "returns", "sets", "gets", "the", "a", "an", "does"} {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, filler))
	}
	rest = strings.Trim(rest, ".:,; \n\t")
	return len([]rune(rest)) >= 12
}

// fieldNames renders a field's names for the "says more than the name" check.
func fieldNames(f *ast.Field) string {
	if len(f.Names) == 0 {
		return ""
	}
	return f.Names[0].Name
}

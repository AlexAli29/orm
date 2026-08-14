package goscan

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/model"
	"golang.org/x/tools/go/packages"
)

// RuntimePkgPath is the import path of this module's runtime package. Relation
// fields are recognised by resolved identity against it, so a type named One in
// any other package is an ordinary field.
const RuntimePkgPath = "github.com/AlexAli29/orm"

// Directive prefixes. A struct is an entity only if it carries one.
const (
	tableDirective    = "//orm:table"
	viewDirective     = "//orm:view"
	matViewDirective  = "//orm:materialized-view"
	defnDirective     = "//orm:definition"
	dependsDirective  = "//orm:depends-on"
	withNoDataDirect  = "//orm:with-no-data"
	withDataDirective = "//orm:with-data"
)

// TagKey is the struct tag key the scanner reads.
const TagKey = "orm"

// Target is one package to scan.
type Target struct {
	// Dir is the absolute directory of the package.
	Dir string
	// OutputDir is the absolute directory generated code for the package would
	// be written to.
	OutputDir string
}

// TagError is a malformed or misapplied `orm` struct tag. It is returned
// alongside the entities rather than as a hard failure, because a bad tag is a
// finding about the author's code, not a failure of the tool.
type TagError struct {
	Entity string
	Field  string
	Tag    string
	Pos    model.Position
	Err    error
}

func (e *TagError) Error() string {
	return fmt.Sprintf("%s.%s: invalid orm tag %q: %v", e.Entity, e.Field, e.Tag, e.Err)
}

func (e *TagError) Unwrap() error { return e.Err }

// Package is one scanned package.
type Package struct {
	Path string
	Name string
	Dir  string
	// Idents maps every package-level identifier to the base name of the file
	// declaring it.
	//
	// A generator needs this to know which names are already taken, and needs
	// the file to tell the author's declarations from its own previous output:
	// on the second run its own identifiers are in scope, and treating those as
	// collisions would make generation succeed exactly once.
	Idents map[string]string
}

// Result is everything the scanner learned.
type Result struct {
	// Entities are sorted by package path and then by type name.
	Entities []*model.GoEntity
	// TagErrors are sorted by position.
	TagErrors []*TagError
	// Packages are sorted by import path.
	Packages []Package
	// Decls are the schema declarations written on types that are not entities
	// — an enum on the named string type that uses it, an extension on
	// whatever type happened to need it. They are package-level schema objects
	// and belong to the schema rather than to one table.
	Decls []model.SchemaDecl
}

// Scan loads targets with full type information and returns the entities they
// declare. Source positions are reported relative to root.
func Scan(ctx context.Context, root string, targets []Target) (*Result, error) {
	if len(targets) == 0 {
		return nil, errors.New("no packages to scan")
	}

	byDir := make(map[string]Target, len(targets))
	patterns := make([]string, 0, len(targets))
	for _, t := range targets {
		dir := filepath.Clean(t.Dir)
		if _, ok := byDir[dir]; ok {
			continue
		}
		byDir[dir] = t
		patterns = append(patterns, dir)
	}

	cfg := &packages.Config{
		Context: ctx,
		Dir:     root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("loading Go packages: %w", err)
	}
	if err := loadErrors(pkgs); err != nil {
		return nil, err
	}

	s := &scanner{root: root, byDir: byDir}
	for _, pkg := range pkgs {
		s.pkg(pkg)
	}
	if s.err != nil {
		return nil, s.err
	}

	slices.SortFunc(s.entities, func(a, b *model.GoEntity) int {
		return cmp.Or(cmp.Compare(a.PkgPath, b.PkgPath), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(s.tagErrors, func(a, b *TagError) int {
		return cmp.Or(
			cmp.Compare(a.Pos.File, b.Pos.File),
			cmp.Compare(a.Pos.Line, b.Pos.Line),
			cmp.Compare(a.Pos.Col, b.Pos.Col),
			cmp.Compare(a.Field, b.Field),
		)
	})
	slices.SortFunc(s.packages, func(a, b Package) int {
		return cmp.Compare(a.Path, b.Path)
	})
	// Declarations are sorted so that the result does not depend on the order
	// packages happened to be walked in.
	slices.SortFunc(s.decls, func(a, b model.SchemaDecl) int {
		return cmp.Or(cmp.Compare(a.GoType, b.GoType), cmp.Compare(a.Name, b.Name))
	})
	return &Result{Entities: s.entities, TagErrors: s.tagErrors, Packages: s.packages, Decls: s.decls}, nil
}

// loadErrors turns package load and type errors into one error.
//
// Entity packages that do not type-check cannot be reconciled: there is nothing
// trustworthy to compare against the schema, and a report derived from
// unresolved types would flag every field of an unknown type as a mismatch the
// author cannot act on. The two failures are worded apart because a package
// that does not exist and a package that does not compile send the reader to
// different places.
func loadErrors(pkgs []*packages.Package) error {
	var errs []error
	var typeErrors bool
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			if e.Kind == packages.TypeError {
				typeErrors = true
			}
			errs = append(errs, fmt.Errorf("%s: %s", pkg.PkgPath, e))
		}
	})
	if len(errs) == 0 {
		return nil
	}
	if typeErrors {
		return fmt.Errorf("the entity packages do not type-check: %w", errors.Join(errs...))
	}
	return fmt.Errorf("the entity packages could not be loaded: %w", errors.Join(errs...))
}

type scanner struct {
	root      string
	byDir     map[string]Target
	entities  []*model.GoEntity
	tagErrors []*TagError
	packages  []Package
	decls     []model.SchemaDecl
	err       error
}

func (s *scanner) pkg(pkg *packages.Package) {
	var pkgDir, outputDir string
	if len(pkg.GoFiles) > 0 {
		pkgDir = filepath.Dir(pkg.GoFiles[0])
		if t, ok := s.byDir[pkgDir]; ok {
			outputDir = t.OutputDir
		}
	}
	s.packages = append(s.packages, Package{
		Path:   pkg.PkgPath,
		Name:   pkg.Name,
		Dir:    pkgDir,
		Idents: s.idents(pkg),
	})
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				// An ungrouped declaration attaches its doc comment to the
				// GenDecl; a declaration inside `type ( ... )` attaches it to
				// the TypeSpec. Both placements are legal Go and both must work.
				doc := ts.Doc
				if doc == nil && len(gen.Specs) == 1 {
					doc = gen.Doc
				}
				d, ok := s.directive(pkg, doc)
				if !ok {
					// A type with no table directive is not an entity, but it
					// may still declare a schema object: an enum lives on the
					// Go type that uses it rather than on a table.
					for _, decl := range s.schemaDecls(pkg, doc) {
						decl.GoType = ts.Name.Name
						s.decls = append(s.decls, decl)
					}
					continue
				}
				s.entity(pkg, ts, d, pkgDir, outputDir)
			}
		}
	}
}

// idents records every package-level identifier and the file it comes from.
func (s *scanner) idents(pkg *packages.Package) map[string]string {
	scope := pkg.Types.Scope()
	out := make(map[string]string, len(scope.Names()))
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		file := ""
		if pos := obj.Pos(); pos.IsValid() && pkg.Fset != nil {
			file = filepath.Base(pkg.Fset.Position(pos).Filename)
		}
		out[name] = file
	}
	return out
}

// directive is a parsed //orm:table or //orm:view comment, together with the
// schema declarations written beside it.
type directive struct {
	table model.TableRef
	kind  model.RelationKind
	view  *model.ViewDecl
	pos   model.Position
	decls []model.SchemaDecl
}

func (s *scanner) directive(pkg *packages.Package, doc *ast.CommentGroup) (directive, bool) {
	if doc == nil {
		return directive{}, false
	}
	for _, c := range doc.List {
		text := strings.TrimRight(c.Text, " \t")
		var arg string
		var kind model.RelationKind
		switch {
		// The materialized directive is tested first: //orm:view is a prefix of
		// nothing, but //orm:materialized-view must not be mistaken for a table
		// by an earlier arm, and testing the longer name first is the habit
		// that keeps that true when another kind is added.
		case strings.HasPrefix(text, matViewDirective):
			arg, kind = strings.TrimSpace(strings.TrimPrefix(text, matViewDirective)), model.RelMaterializedView
		case strings.HasPrefix(text, viewDirective):
			arg, kind = strings.TrimSpace(strings.TrimPrefix(text, viewDirective)), model.RelView
		case strings.HasPrefix(text, tableDirective):
			arg = strings.TrimSpace(strings.TrimPrefix(text, tableDirective))
		default:
			continue
		}
		pos := s.position(pkg, c.Pos())
		if arg == "" {
			s.fail(fmt.Errorf("%s: %s needs a table name; nothing is inferred from the Go type name", pos, strings.Fields(text)[0]))
			return directive{}, false
		}
		if strings.ContainsAny(arg, " \t") {
			s.fail(fmt.Errorf("%s: %q takes exactly one table name", pos, text))
			return directive{}, false
		}
		ref, err := model.ParseTableRef(arg)
		if err != nil {
			s.fail(fmt.Errorf("%s: %w", pos, err))
			return directive{}, false
		}
		d := directive{table: ref, kind: kind, pos: pos, decls: s.schemaDecls(pkg, doc)}
		if kind != model.RelTable {
			d.view = s.viewDecl(pkg, doc, pos)
		}
		return d, true
	}
	return directive{}, false
}

// schemaDecls reads the schema declarations written on a type.
//
// They are read whatever the mode. A database-first project has none, and a
// managed one is not asked to write them somewhere else — which of the two a
// project is decides whether they are used, not whether they are understood.
func (s *scanner) schemaDecls(pkg *packages.Package, doc *ast.CommentGroup) []model.SchemaDecl {
	// A type with no comment has no declarations to read, and most types in a
	// package have no comment. The group is nil rather than empty then, which
	// is the one shape a range over it does not survive.
	if doc == nil {
		return nil
	}
	var out []model.SchemaDecl
	for _, c := range doc.List {
		text := strings.TrimRight(c.Text, " \t")
		if !hasSchemaDirective(text) {
			continue
		}
		pos := s.position(pkg, c.Pos())
		d, ok, err := parseSchemaDecl(text, pos)
		if !ok {
			continue
		}
		if err != nil {
			s.fail(fmt.Errorf("%s: %w", pos, err))
			continue
		}
		out = append(out, d)
	}
	return out
}

func hasSchemaDirective(text string) bool {
	for _, prefix := range schemaDirectives {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func (s *scanner) entity(pkg *packages.Package, ts *ast.TypeSpec, d directive, pkgDir, outputDir string) {
	obj, ok := pkg.TypesInfo.Defs[ts.Name].(*types.TypeName)
	if !ok {
		s.fail(fmt.Errorf("%s: %s has no resolved type", s.position(pkg, ts.Pos()), ts.Name.Name))
		return
	}
	st, ok := obj.Type().Underlying().(*types.Struct)
	if !ok {
		s.fail(fmt.Errorf("%s: %s carries an %s directive but is not a struct", s.position(pkg, ts.Pos()), ts.Name.Name, tableDirective))
		return
	}

	e := &model.GoEntity{
		Name:      ts.Name.Name,
		PkgPath:   pkg.PkgPath,
		PkgName:   pkg.Name,
		Table:     d.table,
		Kind:      d.kind,
		View:      d.view,
		Pos:       s.position(pkg, ts.Pos()),
		Marker:    d.pos,
		Decls:     d.decls,
		PkgDir:    pkgDir,
		OutputDir: outputDir,
	}
	qual := sourceQualifier(pkg.Types)
	for i := range st.NumFields() {
		v := st.Field(i)
		if !v.Exported() {
			// An unexported field cannot be scanned into from another package,
			// so it is not part of the mapped surface.
			continue
		}
		f := model.GoField{
			Name: v.Name(),
			Pos:  s.position(pkg, v.Pos()),
		}
		tags, err := ParseTags(st.Tag(i))
		if err != nil {
			s.tagErrors = append(s.tagErrors, &TagError{
				Entity: e.Display(), Field: v.Name(), Tag: rawTag(st.Tag(i)), Pos: f.Pos, Err: err,
			})
		}
		f.Tags = tags
		f.Rel = relationOf(v.Type())
		f.Type = s.describe(pkg, v.Type(), qual)

		if err := checkTagPlacement(f.Tags, f.Rel != nil); err != nil {
			s.tagErrors = append(s.tagErrors, &TagError{
				Entity: e.Display(), Field: v.Name(), Tag: rawTag(st.Tag(i)), Pos: f.Pos, Err: err,
			})
		}
		e.Fields = append(e.Fields, f)
	}
	s.entities = append(s.entities, e)
}

func (s *scanner) fail(err error) {
	if s.err == nil {
		s.err = err
	}
}

func (s *scanner) position(pkg *packages.Package, p token.Pos) model.Position {
	if !p.IsValid() || pkg.Fset == nil {
		return model.Position{}
	}
	pos := pkg.Fset.Position(p)
	return model.Position{File: s.rel(pos.Filename), Line: pos.Line, Col: pos.Column}
}

func (s *scanner) rel(path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// sourceQualifier renders a type the way it is written in pkg: unqualified for
// pkg's own types and qualified by package name for everything else. It is
// deliberately not types.RelativeTo, which qualifies by import path and would
// report a field as database/sql.NullString rather than sql.NullString.
func sourceQualifier(pkg *types.Package) types.Qualifier {
	return func(other *types.Package) string {
		if other == nil || other == pkg {
			return ""
		}
		return other.Name()
	}
}

// relationOf reports whether t is one of the runtime relation types, resolved
// through go/types. It never looks at how the type was spelled, so an aliased or
// dot import works and a local type called One does not.
func relationOf(t types.Type) *model.GoRel {
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	obj := named.Obj()
	if obj.Pkg() == nil || obj.Pkg().Path() != RuntimePkgPath {
		return nil
	}
	var card model.Cardinality
	switch obj.Name() {
	case "One":
		card = model.CardOne
	case "Many":
		card = model.CardMany
	default:
		return nil
	}
	args := named.TypeArgs()
	if args == nil || args.Len() != 1 {
		return nil
	}
	return &model.GoRel{Cardinality: card, Target: qualifiedName(args.At(0))}
}

// qualifiedName renders a type argument the way reconciliation identifies an
// entity. A target that is not a named type keeps its full rendering so the
// resulting finding can quote it back.
func qualifiedName(t types.Type) string {
	if named, ok := t.(*types.Named); ok {
		obj := named.Obj()
		if obj.Pkg() != nil {
			return obj.Pkg().Path() + "." + obj.Name()
		}
		return obj.Name()
	}
	return types.TypeString(t, nil)
}

// describe resolves a field's type into the neutral model.
func (s *scanner) describe(pkg *packages.Package, t types.Type, qual types.Qualifier) model.GoType {
	gt := model.GoType{Src: types.TypeString(t, qual), SrcRefs: referencedPackages(t)}

	cur := t
	if ptr, ok := cur.Underlying().(*types.Pointer); ok {
		gt.Ptr = true
		cur = ptr.Elem()
	}
	if elem, ok := sqlNullElem(cur); ok {
		gt.SQLNull = true
		cur = elem
	}

	if named, ok := cur.(*types.Named); ok {
		obj := named.Obj()
		if obj.Pkg() != nil {
			gt.Named = obj.Pkg().Path() + "." + obj.Name()
		} else {
			gt.Named = obj.Name()
		}
		// An instantiation's object is the origin's, so Named alone cannot
		// tell Range[int32] from Range[int64]. Go keeps the arguments; this
		// records them, in declaration order, described the same way every
		// other type is.
		if args := named.TypeArgs(); args != nil {
			gt.TypeArgs = make([]model.GoType, 0, args.Len())
			for i := range args.Len() {
				gt.TypeArgs = append(gt.TypeArgs, s.describe(pkg, args.At(i), qual))
			}
		}
	}

	// Value is the type a predicate compares against, which is what remains
	// once the two null-carrying wrappers are gone.
	gt.Value = types.TypeString(cur, qual)
	gt.Refs = referencedPackages(cur)

	gt.Kind = classify(cur, gt.Named)
	switch u := cur.Underlying().(type) {
	case *types.Slice:
		elem := s.describe(pkg, u.Elem(), qual)
		gt.Elem = &elem
	}
	if gt.Named != "" && (gt.Kind == model.KindString || gt.Kind.IsInteger()) {
		gt.Enum = s.enumConsts(pkg, cur)
	}
	return gt
}

// referencedPackages returns, sorted and deduplicated, the import paths every
// named type inside t belongs to.
//
// A generated file has to import exactly what the types it writes down need:
// too few and it does not compile, too many and gofmt-clean output still fails
// vet. Walking the resolved type is the only way to know, since the rendered
// string says "time.Time" without saying where time is.
func referencedPackages(t types.Type) []string {
	seen := make(map[string]bool)
	var walk func(types.Type, int)
	walk = func(t types.Type, depth int) {
		// Named types can be mutually recursive; the depth cap ends the walk
		// without needing to track every type visited.
		if t == nil || depth > 16 {
			return
		}
		switch t := t.(type) {
		case *types.Named:
			if obj := t.Obj(); obj.Pkg() != nil {
				seen[obj.Pkg().Path()] = true
			}
			for i := range t.TypeArgs().Len() {
				walk(t.TypeArgs().At(i), depth+1)
			}
		case *types.Pointer:
			walk(t.Elem(), depth+1)
		case *types.Slice:
			walk(t.Elem(), depth+1)
		case *types.Array:
			walk(t.Elem(), depth+1)
		case *types.Map:
			walk(t.Key(), depth+1)
			walk(t.Elem(), depth+1)
		}
	}
	walk(t, 0)

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// classify maps a resolved type onto a model kind.
func classify(t types.Type, named string) model.GoKind {
	if named == "time.Time" {
		return model.KindTime
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return basicKind(u)
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
			return model.KindBytes
		}
		return model.KindSlice
	case *types.Map:
		return model.KindMap
	case *types.Struct:
		return model.KindStruct
	case *types.Interface:
		if u.Empty() {
			return model.KindAny
		}
		return model.KindUnsupported
	default:
		return model.KindUnsupported
	}
}

var basicKinds = map[types.BasicKind]model.GoKind{
	types.Bool:    model.KindBool,
	types.Int:     model.KindInt,
	types.Int8:    model.KindInt8,
	types.Int16:   model.KindInt16,
	types.Int32:   model.KindInt32,
	types.Int64:   model.KindInt64,
	types.Uint:    model.KindUint,
	types.Uint8:   model.KindUint8,
	types.Uint16:  model.KindUint16,
	types.Uint32:  model.KindUint32,
	types.Uint64:  model.KindUint64,
	types.Float32: model.KindFloat32,
	types.Float64: model.KindFloat64,
	types.String:  model.KindString,
}

func basicKind(b *types.Basic) model.GoKind {
	if k, ok := basicKinds[b.Kind()]; ok {
		return k
	}
	return model.KindUnsupported
}

// preGenericNulls maps the database/sql null structs that predate sql.Null[T]
// onto the type they wrap.
var preGenericNulls = map[string]string{
	"NullBool":    "bool",
	"NullByte":    "byte",
	"NullFloat64": "float64",
	"NullInt16":   "int16",
	"NullInt32":   "int32",
	"NullInt64":   "int64",
	"NullString":  "string",
	"NullTime":    "time.Time",
}

// sqlNullElem reports the value type wrapped by a database/sql null type.
func sqlNullElem(t types.Type) (types.Type, bool) {
	named, ok := t.(*types.Named)
	if !ok {
		return nil, false
	}
	obj := named.Obj()
	if obj.Pkg() == nil || obj.Pkg().Path() != "database/sql" {
		return nil, false
	}
	if obj.Name() == "Null" {
		if args := named.TypeArgs(); args != nil && args.Len() == 1 {
			return args.At(0), true
		}
		return nil, false
	}
	if _, ok := preGenericNulls[obj.Name()]; !ok {
		return nil, false
	}
	// The pre-generic wrappers are structs whose first field holds the value.
	st, ok := named.Underlying().(*types.Struct)
	if !ok || st.NumFields() == 0 {
		return nil, false
	}
	return st.Field(0).Type(), true
}

// enumConsts collects the typed constants declared for a named type, which is
// what an enum column's labels are reconciled against.
func (s *scanner) enumConsts(pkg *packages.Package, t types.Type) []model.EnumConst {
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	obj := named.Obj()
	if obj.Pkg() == nil {
		return nil
	}
	scope := obj.Pkg().Scope()
	var out []model.EnumConst
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok || !types.Identical(c.Type(), named) {
			continue
		}
		ec := model.EnumConst{Name: name, Pos: s.position(pkg, c.Pos())}
		switch v := c.Val(); v.Kind() {
		case constant.String:
			ec.IsString, ec.Str = true, constant.StringVal(v)
		case constant.Int:
			i, exact := constant.Int64Val(v)
			if !exact {
				continue
			}
			ec.Int = i
		default:
			continue
		}
		out = append(out, ec)
	}
	slices.SortFunc(out, func(a, b model.EnumConst) int {
		return cmp.Or(
			cmp.Compare(a.Pos.File, b.Pos.File),
			cmp.Compare(a.Pos.Line, b.Pos.Line),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return out
}

// rawTag returns the `orm` tag as written, for quoting in diagnostics.
func rawTag(tag string) string {
	v, _ := lookupTag(tag, TagKey)
	return v
}

// lookupTag is the standard struct tag conventions, spelled out here because
// reflect.StructTag operates on a reflect type this package never has.
func lookupTag(tag, key string) (string, bool) {
	for tag != "" {
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}
		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := tag[:i]
		tag = tag[i+1:]

		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		quoted := tag[:i+1]
		tag = tag[i+1:]

		if name == key {
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

package model

import (
	"fmt"
	"github.com/AlexAli29/orm/internal/gen/eligible"
	"github.com/AlexAli29/orm/postgis"
	"strings"
)

// Position is a source location, reported relative to the project root so that
// diagnostics never contain machine-specific absolute paths.
type Position struct {
	File string
	Line int
	Col  int
}

// String renders the position as file:line:col, or the empty string when the
// position is unknown.
func (p Position) String() string {
	if p.File == "" {
		return ""
	}
	if p.Line == 0 {
		return p.File
	}
	if p.Col == 0 {
		return fmt.Sprintf("%s:%d", p.File, p.Line)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// IsZero reports whether the position is unknown.
func (p Position) IsZero() bool { return p.File == "" }

// TableRef names a PostgreSQL table as written in an //orm:table directive.
// Schema is empty when the directive gave a bare name, in which case the
// configured search_path resolves it.
type TableRef struct {
	Schema string
	Name   string
}

// String renders the reference as written, qualifying it only when the
// directive did.
func (t TableRef) String() string {
	if t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// ParseTableRef splits a directive argument into an optional schema and a name.
// It rejects empty parts and anything with more than one dot; quoting and
// case-folding are deliberately not supported in v1.
func ParseTableRef(s string) (TableRef, error) {
	switch schema, name, found := strings.Cut(s, "."); {
	case s == "":
		return TableRef{}, fmt.Errorf("empty table name")
	case !found:
		return TableRef{Name: s}, nil
	case schema == "" || name == "":
		return TableRef{}, fmt.Errorf("malformed qualified table name %q", s)
	case strings.Contains(name, "."):
		return TableRef{}, fmt.Errorf("table name %q has more than one qualifier", s)
	default:
		return TableRef{Schema: schema, Name: name}, nil
	}
}

// GoKind classifies a Go type after stripping one level of pointer, unwrapping a
// database/sql null wrapper and resolving named types to their underlying kind.
type GoKind uint8

// Go kinds recognised by the reconciler. Anything the scanner cannot place lands
// on KindUnsupported, which no PostgreSQL type will match.
const (
	KindUnsupported GoKind = iota
	KindBool
	KindInt
	KindInt8
	KindInt16
	KindInt32
	KindInt64
	KindUint
	KindUint8
	KindUint16
	KindUint32
	KindUint64
	KindFloat32
	KindFloat64
	KindString
	KindBytes // []byte, including named types whose underlying type is []byte
	KindSlice
	KindMap
	KindStruct
	KindAny
	KindTime // time.Time
)

var goKindNames = map[GoKind]string{
	KindUnsupported: "unsupported",
	KindBool:        "bool",
	KindInt:         "int",
	KindInt8:        "int8",
	KindInt16:       "int16",
	KindInt32:       "int32",
	KindInt64:       "int64",
	KindUint:        "uint",
	KindUint8:       "uint8",
	KindUint16:      "uint16",
	KindUint32:      "uint32",
	KindUint64:      "uint64",
	KindFloat32:     "float32",
	KindFloat64:     "float64",
	KindString:      "string",
	KindBytes:       "[]byte",
	KindSlice:       "slice",
	KindMap:         "map",
	KindStruct:      "struct",
	KindAny:         "any",
	KindTime:        "time.Time",
}

// String returns the kind's name for use in diagnostics.
func (k GoKind) String() string {
	if s, ok := goKindNames[k]; ok {
		return s
	}
	return "unknown"
}

// IsInteger reports whether the kind is one of the sized or unsized integers.
func (k GoKind) IsInteger() bool { return k >= KindInt && k <= KindUint64 }

// EnumConst is a typed constant discovered alongside a named Go string or
// integer type. It is what an enum column's labels are reconciled against.
type EnumConst struct {
	Name     string
	Str      string
	Int      int64
	IsString bool
	Pos      Position
}

// Value renders the constant's value the way it would appear in PostgreSQL.
func (c EnumConst) Value() string {
	if c.IsString {
		return c.Str
	}
	return fmt.Sprintf("%d", c.Int)
}

// GoType is a resolved description of a struct field's type, produced from
// go/types rather than from source text.
//
// Kind, Named and Elem describe the type after removing one pointer level and
// any database/sql null wrapper, because those two constructions affect
// nullability rather than the underlying value's shape. Src preserves the type
// as written so diagnostics can quote it back to the author.
type GoType struct {
	// Src is the type as written in the source, qualified by package name.
	Src string
	// Value is the value type rendered the same way, after removing one
	// pointer level and any sql null wrapper. For a *string field it is
	// "string"; for a plain string field it is the same as Src.
	//
	// It exists because a nullable column is compared against a value, not
	// against a pointer: a descriptor for *string offers Eq(string).
	Value string
	// Refs lists, sorted, the package paths the value type mentions, so that a
	// generated file can import exactly what it needs and nothing else.
	Refs []string
	// SrcRefs is the same for the declared type, which differs whenever a null
	// wrapper is involved: sql.Null[string] names database/sql where the value
	// behind it names nothing.
	SrcRefs []string
	// Ptr reports whether the field's declared type is a pointer.
	Ptr bool
	// SQLNull reports whether the field's declared type is database/sql.Null[T]
	// or one of the pre-generics sql.NullXxx structs.
	SQLNull bool
	// Kind classifies the value type once Ptr and SQLNull are peeled away.
	Kind GoKind
	// Named is the fully qualified name of the value's named type
	// ("time.Time", "github.com/shopspring/decimal.Decimal"), or empty when the
	// value type is unnamed.
	Named string
	// TypeArgs holds the type arguments of a generic instantiation, in
	// declaration order, and is empty for every non-generic type.
	//
	// It matters because Named is the *origin's* name: Range[int32] and
	// Range[int64] are both "…/orm.Range", and without the arguments the two
	// would be one type to every check downstream. Go keeps them —
	// types.Named.TypeArgs() — and this is where they stop being thrown away.
	//
	// The arguments are themselves GoTypes, so a nested instantiation such as
	// Range[Box[int32]] describes all the way down.
	TypeArgs []GoType
	// Elem describes the element type of a slice or array value, and is nil for
	// every other kind. Byte slices report KindBytes and leave Elem set so that
	// bytea and array reconciliation can share one path.
	Elem *GoType
	// Enum holds the typed constants declared for Named, in source order. It is
	// empty unless the named type's underlying type is a string or an integer
	// and at least one constant was declared.
	Enum []EnumConst
}

// String renders the type as written.
func (t GoType) String() string { return t.Src }

// Instantiated reports whether the type is a generic instantiation.
func (t GoType) Instantiated() bool { return len(t.TypeArgs) > 0 }

// Generic renders the type's identity including its type arguments, which is
// what distinguishes two instantiations of one generic origin.
//
// An unnamed type — a basic like int32, or a slice — has no qualified name to
// render, so it contributes what it was written as. That only ever happens
// inside the brackets: the thing being instantiated is always named.
//
// It is a new reading of the same data rather than a replacement for [GoType.Named],
// which is unchanged. Nothing that fingerprinted a non-generic type before
// generics were understood sees a different value.
func (t GoType) Generic() string {
	name := t.Named
	if name == "" {
		name = t.Src
	}
	if len(t.TypeArgs) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('[')
	for i, a := range t.TypeArgs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(a.Generic())
	}
	b.WriteByte(']')
	return b.String()
}

// Nullable reports whether the Go type can represent SQL NULL distinctly from a
// value.
//
// Pointers and sql null wrappers obviously can. Maps and any can, because their
// nil is distinguishable from every value the reconciler will map onto them. A
// plain slice cannot: a nil []string and an empty []string are both legal
// values of an array column, so NULL has nowhere to go. A nullable array
// therefore needs *[]string.
func (t GoType) Nullable() bool {
	switch {
	case t.Ptr, t.SQLNull:
		return true
	case t.Kind == KindMap, t.Kind == KindAny:
		return true
	default:
		return false
	}
}

// IsEnum reports whether the type is a named type with discovered constants.
func (t GoType) IsEnum() bool { return t.Named != "" && len(t.Enum) > 0 }

// FieldTags is a parsed `orm:"..."` struct tag.
type FieldTags struct {
	// Ignore is set by the "-" tag and excludes the field from reconciliation.
	Ignore bool
	// Column overrides the derived column name.
	Column string
	// FK names the PostgreSQL constraint a relation must use.
	FK string
	// Side, valid only when HasSide is set, pins the relation's foreign key
	// side instead of letting reconciliation derive it.
	Side    FKSide
	HasSide bool
	// OnDelete and OnUpdate carry the relation's referential actions, as the
	// canonical SQL spelling ("CASCADE", "SET NULL"). Empty means the tag said
	// nothing, which is PostgreSQL's default of NO ACTION.
	//
	// They are read only in managed mode, like the rest of the desired-schema
	// directives below. A database-first project takes whatever the constraint
	// already says, because PostgreSQL owns it.
	OnDelete string
	OnUpdate string
	// Type selects a configured type mapping by key.
	Type string

	// The rest describe desired schema and are read only in managed mode. A
	// database-first project never writes them, and reconciliation ignores
	// them: PostgreSQL is what it is, whatever a tag would have asked for.

	// PK marks the field as part of the table's primary key. Several fields
	// carrying it form a composite key, in field order.
	PK bool
	// Identity records GENERATED ... AS IDENTITY. HasIdentity separates "not an
	// identity column" from the zero value of the kind.
	Identity    IdentityKind
	HasIdentity bool
	// Unique asks for a single-column unique constraint.
	Unique bool
	// Default is the column's DEFAULT expression, as SQL. It is never derived
	// from a Go zero value.
	Default    string
	HasDefault bool
	// Generated is a stored generated column's expression, as SQL.
	Generated string
	// PGType names the PostgreSQL type directly, for the cases where the
	// default for the Go type is not the one wanted.
	PGType string

	// Raw is the tag as written, for diagnostics.
	Raw string
}

// IdentityKind is how PostgreSQL supplies an identity column's value.
type IdentityKind uint8

const (
	// IdentityByDefault is GENERATED BY DEFAULT AS IDENTITY, which accepts an
	// explicit value.
	IdentityByDefault IdentityKind = iota
	// IdentityAlways is GENERATED ALWAYS AS IDENTITY.
	IdentityAlways
)

// RelationKind is what an entity's directive declared.
type RelationKind uint8

const (
	// RelTable is //orm:table.
	RelTable RelationKind = iota
	// RelView is //orm:view.
	RelView
	// RelMaterializedView is //orm:materialized-view.
	RelMaterializedView
)

// String returns the kind as PostgreSQL names it.
func (k RelationKind) String() string {
	switch k {
	case RelView:
		return "view"
	case RelMaterializedView:
		return "materialized view"
	default:
		return "table"
	}
}

// Directive returns the directive that declares this kind.
func (k RelationKind) Directive() string {
	switch k {
	case RelView:
		return "//orm:view"
	case RelMaterializedView:
		return "//orm:materialized-view"
	default:
		return "//orm:table"
	}
}

// ViewDecl is everything a managed view declaration carries beyond its columns.
//
// A view without a definition is not a view: there is nothing to create. The
// scanner therefore records the definition here and the desired-schema builder
// refuses a declaration that has none, rather than discovering it while
// rendering DDL.
type ViewDecl struct {
	// Mode says where the definition came from.
	Mode DefinitionMode
	// SQL is the definition itself for an inline declaration, and the file's
	// contents once a referenced file has been read.
	SQL string
	// File is the path exactly as the directive wrote it, relative to the
	// package directory. It is kept for diagnostics and for the lock, and it is
	// never absolute: an absolute path would make the lock depend on where a
	// checkout happens to sit.
	File string
	// DependsOn are the relations the definition reads, as the declaration
	// states them.
	//
	// They are authoritative for managed ordering. Nothing reads the SQL to
	// find them: a definition may name a relation inside a CTE, behind a
	// function, through a quoted identifier or not at all, and a text search
	// that guessed would be wrong in whichever direction is least safe.
	DependsOn []TableRef
	// WithNoData asks for CREATE MATERIALIZED VIEW ... WITH NO DATA.
	//
	// It is creation policy, not runtime state: it says how the relation should
	// be initialised, never whether it currently holds rows. The two are
	// different questions and a single boolean answering both would report
	// drift every time somebody refreshed.
	WithNoData bool
	// Pos is where the definition was declared.
	Pos Position
}

// DefinitionMode is where a managed view's definition comes from.
type DefinitionMode uint8

const (
	// DefinitionNone is a declaration with no definition, which is an error the
	// desired-schema builder reports.
	DefinitionNone DefinitionMode = iota
	// DefinitionInlineSQL is SQL written in the directive itself.
	DefinitionInlineSQL
	// DefinitionFileSQL is SQL read from a file the directive names.
	DefinitionFileSQL
)

// String returns the mode's name.
func (m DefinitionMode) String() string {
	switch m {
	case DefinitionInlineSQL:
		return "inline SQL"
	case DefinitionFileSQL:
		return "SQL file"
	default:
		return "none"
	}
}

// SchemaDecl is one struct-level schema declaration.
//
// Indexes, checks and multi-column constraints live here rather than in field
// tags, because a tag that had to describe a two-column partial covering index
// would be a small unreadable language of its own. They name Go fields, which
// the scanner resolves and validates, so nothing here refers to generated code
// and a fresh project can declare its schema before anything is generated.
type SchemaDecl struct {
	Kind SchemaDeclKind
	// Name is the object's name, empty when it should be derived.
	Name string
	// Fields are the Go field names the declaration refers to, in order.
	Fields []SchemaDeclField
	// Include names the covering columns of an index.
	Include []string
	// Expr is a check condition or an index predicate, as SQL.
	Expr string
	// Method is an index access method.
	Method string
	// Unique marks a unique index.
	Unique bool
	// Concurrently records how the index should be created, which is migration
	// metadata rather than schema state.
	Concurrently bool
	// Labels are an enum's labels, in order.
	Labels []string
	// GoType is the Go type the declaration was written on, for a package-level
	// declaration such as an enum. It is empty on an entity's own declarations.
	GoType string
	// Raw is the directive as written, for diagnostics.
	Raw string
	Pos Position
}

// SchemaDeclField is one field reference in a declaration, with its ordering.
type SchemaDeclField struct {
	// Field is the Go field name, or an expression when Expression is set.
	Field string
	// Expression makes the key an SQL expression rather than a column.
	Expression string
	Desc       bool
	NullsFirst bool
	NullsLast  bool
	OpClass    string
}

// SchemaDeclKind is what a struct-level declaration declares.
type SchemaDeclKind uint8

const (
	DeclIndex SchemaDeclKind = iota
	DeclUnique
	DeclCheck
	DeclEnum
	DeclExtension
)

func (k SchemaDeclKind) String() string {
	switch k {
	case DeclUnique:
		return "unique"
	case DeclCheck:
		return "check"
	case DeclEnum:
		return "enum"
	case DeclExtension:
		return "extension"
	default:
		return "index"
	}
}

// Cardinality distinguishes the two relation shapes an entity may declare. It is
// a property of the Go field — orm.One or orm.Many — and says nothing about
// which table holds the foreign key.
type Cardinality uint8

// Relation cardinalities.
const (
	CardOne Cardinality = iota
	CardMany
)

// String returns "one" or "many".
func (c Cardinality) String() string {
	if c == CardMany {
		return "many"
	}
	return "one"
}

// FKSide records which table declares the foreign key backing a relation. It is
// derived during reconciliation from the catalog, never declared in Go, except
// when an ambiguity forces the author to pin it with a side: tag.
type FKSide uint8

// Foreign key sides, relative to the entity that declares the relation.
const (
	// FKLocal means the declaring entity's own table carries the foreign key.
	FKLocal FKSide = iota
	// FKRemote means the target entity's table carries the foreign key.
	FKRemote
)

// String returns "local" or "remote".
func (s FKSide) String() string {
	if s == FKRemote {
		return "remote"
	}
	return "local"
}

// ParseFKSide parses a side: tag value.
func ParseFKSide(s string) (FKSide, error) {
	switch s {
	case "local":
		return FKLocal, nil
	case "remote":
		return FKRemote, nil
	default:
		return 0, fmt.Errorf("invalid side %q, want local or remote", s)
	}
}

// GoRel marks a field as a relation. It deliberately carries no direction:
// direction is a fact about the schema, and the schema is introspected, not
// declared.
type GoRel struct {
	Cardinality Cardinality
	// Target is the fully qualified name of the related Go type, for example
	// "example.com/app/internal/domain.User".
	Target string
}

// GoField is one struct field of an entity.
type GoField struct {
	Name string
	Type GoType
	Tags FieldTags
	// Rel is non-nil exactly when the field's type is orm.One[T] or
	// orm.Many[T], in which case Type describes the relation wrapper and is not
	// reconciled against a column.
	Rel *GoRel
	Pos Position
}

// IsRelation reports whether the field declares a relation.
func (f *GoField) IsRelation() bool { return f.Rel != nil }

// GoEntity is a struct carrying an //orm:table or //orm:view directive.
type GoEntity struct {
	Name    string
	PkgPath string
	PkgName string
	// Table is the reference written in the directive. No name is inferred: an
	// entity without a directive is not an entity, and a directive without an
	// argument is an error.
	Table TableRef
	// Kind is what the directive declared: a table, an ordinary view or a
	// materialized view.
	//
	// It is a kind rather than a pair of booleans because the three are
	// mutually exclusive and carry different capabilities. A boolean per kind
	// would make "a view that is also materialized" representable, and the
	// first code path that read only one of them would decide the other did not
	// matter.
	Kind RelationKind
	// View is the view's definition and dependencies, and is nil on a table.
	View   *ViewDecl
	Fields []GoField
	// Pos is the position of the type declaration; Marker is the position of
	// the directive comment.
	Pos    Position
	Marker Position
	// PkgDir is the directory the entity's package lives in.
	PkgDir string
	// OutputDir is the directory generated code for this entity would be
	// written to, resolved from the package configuration.
	OutputDir string
	// Decls are the struct-level schema declarations, in the order they were
	// written. Managed mode reads them; database mode ignores them.
	Decls []SchemaDecl
}

// Qualified returns the entity's fully qualified Go type name.
func (e *GoEntity) Qualified() string { return e.PkgPath + "." + e.Name }

// Display returns a short name for diagnostics, qualified by package name only.
func (e *GoEntity) Display() string {
	if e.PkgName == "" {
		return e.Name
	}
	return e.PkgName + "." + e.Name
}

// PGTypeKind mirrors pg_type.typtype.
type PGTypeKind uint8

// PostgreSQL type kinds.
const (
	PGBase PGTypeKind = iota
	PGEnum
	PGDomain
	PGComposite
	PGRange
	PGMultirange
	PGPseudo
	PGArray // a base type whose category is A and which has an element type
)

var pgTypeKindNames = map[PGTypeKind]string{
	PGBase:       "base",
	PGEnum:       "enum",
	PGDomain:     "domain",
	PGComposite:  "composite",
	PGRange:      "range",
	PGMultirange: "multirange",
	PGPseudo:     "pseudo",
	PGArray:      "array",
}

// String returns the kind's name.
func (k PGTypeKind) String() string {
	if s, ok := pgTypeKindNames[k]; ok {
		return s
	}
	return "unknown"
}

// PGType is a type read from pg_type, with its element or base type resolved.
type PGType struct {
	OID    uint32
	Schema string
	Name   string
	Kind   PGTypeKind
	// Elem is the element type of an array or the base type of a domain.
	Elem *PGType
	// Labels holds an enum's labels in enumsortorder.
	Labels []string
	// DomainNotNull records a domain declared NOT NULL, which makes every
	// column of that domain non-nullable regardless of the column's own
	// attnotnull.
	DomainNotNull bool
}

// String renders the type name, qualifying it unless it lives in pg_catalog.
func (t *PGType) String() string {
	if t == nil {
		return "<unknown>"
	}
	if t.Kind == PGArray && t.Elem != nil {
		return t.Elem.String() + "[]"
	}
	if t.Schema == "pg_catalog" || t.Schema == "" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// Key returns the name used to look a type up in the configured type mappings:
// the bare name for pg_catalog types and the qualified name otherwise.
func (t *PGType) Key() string { return t.String() }

// Resolve follows domains down to the type that actually determines the value's
// shape. Arrays are not followed, because an array is a distinct shape.
func (t *PGType) Resolve() *PGType {
	for t != nil && t.Kind == PGDomain && t.Elem != nil {
		t = t.Elem
	}
	return t
}

// PGColumn is one attribute of a table.
type PGColumn struct {
	Table   *PGTable
	Name    string
	AttNum  int
	Type    *PGType
	NotNull bool
	// HasDefault reports a column default, including the sequence default of a
	// serial column but excluding identity and generated columns, which are
	// reported separately.
	HasDefault bool
	// Identity is 'a' for GENERATED ALWAYS AS IDENTITY, 'd' for BY DEFAULT, and
	// zero otherwise.
	Identity byte
	// Generated is 's' for a stored generated column and zero otherwise.
	Generated byte
	// Dims is pg_attribute.attndims; anything above 1 is a multidimensional
	// array, which v1 does not map.
	Dims int
	// Formatted is format_type(atttypid, atttypmod): the type as PostgreSQL
	// itself writes it, with the modifier the catalog's type name does not
	// carry.
	//
	// It exists because a type modifier can be load-bearing. varchar(255) and
	// varchar differ only in a length nobody has to check, but
	// geometry(Point,4326) and geometry(MultiPolygon,3857) are different columns
	// holding different things, and the difference lives entirely here.
	Formatted string
}

// Spatial reports the column's PostGIS declaration, and whether it has one.
//
// The modifier is parsed rather than matched, because the three facts inside it
// — shape, dimensionality, coordinate system — are each something a check or a
// generated descriptor needs on its own.
func (c *PGColumn) Spatial() (postgis.TypeMod, bool) {
	m, err := postgis.ParseTypeMod(c.Formatted)
	if err != nil || !m.Spatial() {
		return postgis.TypeMod{}, false
	}
	return m, true
}

// Qualified renders the column as table.column, schema-qualified.
func (c *PGColumn) Qualified() string {
	if c.Table == nil {
		return c.Name
	}
	return c.Table.Qualified() + "." + c.Name
}

// IsIdentity reports whether the column is an identity column.
func (c *PGColumn) IsIdentity() bool { return c.Identity == 'a' || c.Identity == 'd' }

// IsGenerated reports whether the column is a stored generated column.
func (c *PGColumn) IsGenerated() bool { return c.Generated != 0 }

// Writable reports whether an INSERT may supply a value for the column.
func (c *PGColumn) Writable() bool { return !c.IsGenerated() && c.Identity != 'a' }

// Suppliable reports whether a row can be inserted without naming the column.
//
// It asks Nullable rather than reading NotNull directly, because a column of a
// NOT NULL domain rejects an omitted value exactly as a NOT NULL column does,
// and an insert that fails on a domain constraint fails just as completely.
func (c *PGColumn) Suppliable() bool {
	return c.Nullable() || c.HasDefault || c.IsIdentity() || c.IsGenerated()
}

// Nullable reports whether the column may hold NULL, taking a NOT NULL domain
// into account.
func (c *PGColumn) Nullable() bool {
	if c.NotNull {
		return false
	}
	for t := c.Type; t != nil && t.Kind == PGDomain; t = t.Elem {
		if t.DomainNotNull {
			return false
		}
	}
	return true
}

// PGUnique is a unique index on a table.
type PGUnique struct {
	Name string
	Cols []*PGColumn
	// Partial reports an index with a WHERE clause. A partial unique index
	// constrains only the rows it covers and is therefore never proof that a
	// column is globally unique.
	Partial bool
	// Expression reports an index over expressions rather than plain columns,
	// whose Cols list is incomplete and cannot be used as proof either.
	Expression bool
	// Primary reports the index backing the primary key constraint.
	Primary bool
}

// Total reports whether the index proves global uniqueness of its columns.
func (u PGUnique) Total() bool { return !u.Partial && !u.Expression && len(u.Cols) > 0 }

// PGForeignKey is a foreign key constraint. Cols and RefCols are stored in
// conkey and confkey order and are aligned pairwise; the ordering is load
// bearing and must never be compared as a set.
type PGForeignKey struct {
	Name     string
	Table    *PGTable
	RefTable *PGTable
	Cols     []*PGColumn
	RefCols  []*PGColumn
}

// PGTable is a table (relkind 'r') or partitioned table (relkind 'p').
type PGTable struct {
	OID    uint32
	Schema string
	Name   string
	Kind   byte
	Cols   []*PGColumn
	// PK holds the primary key columns in constraint order, and is empty when
	// the table has no primary key.
	PK     []*PGColumn
	PKName string
	// FKs holds the foreign keys declared on this table, sorted by name.
	FKs []*PGForeignKey
	// Uniques holds every unique index on this table, sorted by name, including
	// partial and expression indexes so that reconciliation can explain why an
	// index was not accepted as proof.
	Uniques []PGUnique
	// Definition is what PostgreSQL says a view or materialized view selects,
	// read back with pg_get_viewdef. It is empty for a table.
	//
	// It is a reconstruction, not the text anybody typed: PostgreSQL parses a
	// definition, stores the parsed query and deparses it on request, so the
	// result requalifies every name, expands *, adds casts and drops comments.
	// Nothing here should ever claim it is the original DDL.
	Definition string
	// DependsOn are the relations this view's definition reads, sorted by
	// qualified name, read from pg_depend through the view's rewrite rule.
	DependsOn []PGRelationRef
	// Options are the view options the catalog records — reloptions such as
	// security_barrier and security_invoker, and the check option. They are
	// carried rather than interpreted.
	Options []PGViewOption
	// Populated is whether a materialized view currently has data. It is
	// runtime state: a view created or refreshed WITH NO DATA is unscannable
	// until it is refreshed again.
	Populated bool
	// Unrepresented names catalog metadata this relation carries that the
	// schema model cannot express. It is reported rather than dropped: silently
	// discarding a storage parameter or an unknown option is how a roundtrip
	// changes what a database means.
	Unrepresented []string
}

// PGRelationRef is one relation a view definition depends on.
type PGRelationRef struct {
	Schema string
	Name   string
	// Kind is the catalog's relkind: r, p, v, m, f.
	Kind byte
}

// PGViewOption is one view option as the catalog records it.
type PGViewOption struct {
	Name  string
	Value string
}

// IsView reports a relation whose contents are a stored query.
func (t *PGTable) IsView() bool { return t.Kind == 'v' || t.Kind == 'm' }

// IsMaterialized reports a materialized view.
func (t *PGTable) IsMaterialized() bool { return t.Kind == 'm' }

// Writable reports whether an entity may be written through this relation.
//
// Only an ordinary or partitioned table. PostgreSQL will accept writes through
// some views, and through any view with an INSTEAD OF trigger, but generating a
// write API that depends on the shape of a definition would be a promise the
// generator cannot keep by looking at the catalog — so views are read-only, and
// the absence of the method is the API rather than an error at runtime.
func (t *PGTable) Writable() bool { return t.Kind == 'r' || t.Kind == 'p' }

// Qualified renders the table as schema.name.
func (t *PGTable) Qualified() string { return t.Schema + "." + t.Name }

// Column returns the named column, or nil.
func (t *PGTable) Column(name string) *PGColumn {
	for _, c := range t.Cols {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Schema is the introspected subset of a database.
type Schema struct {
	// SearchPath is the schema list the introspection covered, in order.
	SearchPath []string
	// Tables holds every introspected table sorted by qualified name.
	//
	// Views and materialized views are not in here. They are relations a query
	// can read, and they are not tables: nothing may be written through one,
	// they carry no constraints, and the code that plans a migration for a
	// table would produce ALTER TABLE against a stored query. Keeping them in
	// one slice with a kind byte to check would work exactly until the first
	// reader forgot to check.
	Tables []*PGTable
	// Views holds every introspected ordinary and materialized view, sorted by
	// qualified name. The kind is on each one.
	Views  []*PGTable
	byName map[string]*PGTable
}

// NewSchema builds a Schema over tables, which it takes ownership of. The caller
// is responsible for having sorted them.
func NewSchema(searchPath []string, tables []*PGTable) *Schema {
	return NewSchemaWithViews(searchPath, tables, nil)
}

// NewSchemaWithViews builds a schema that also knows its views.
//
// Every relation is in byName, tables and views alike, because PostgreSQL has
// one namespace for them: a schema cannot hold a table and a view of the same
// name, and a lookup that missed a view would report the name as free when
// creating anything there would fail.
func NewSchemaWithViews(searchPath []string, tables, views []*PGTable) *Schema {
	s := &Schema{
		SearchPath: searchPath,
		Tables:     tables,
		Views:      views,
		byName:     make(map[string]*PGTable, len(tables)+len(views)),
	}
	for _, t := range tables {
		s.byName[t.Qualified()] = t
	}
	for _, v := range views {
		s.byName[v.Qualified()] = v
	}
	return s
}

// LookupTable resolves a reference and returns it only when it is a table.
//
// It is what any caller wanting to write, constrain or migrate should use. The
// second result says whether the name exists at all, so a caller can tell "no
// such relation" from "that is a view", which are different mistakes needing
// different messages.
func (s *Schema) LookupTable(ref TableRef) (t *PGTable, isTable, exists bool) {
	r, ok := s.Lookup(ref)
	if !ok {
		return nil, false, false
	}
	if !r.Writable() {
		return r, false, true
	}
	return r, true, true
}

// Lookup resolves a table reference. An unqualified reference is resolved
// against the search path in order, matching PostgreSQL's own rule.
func (s *Schema) Lookup(ref TableRef) (*PGTable, bool) {
	if s == nil {
		return nil, false
	}
	if ref.Schema != "" {
		t, ok := s.byName[ref.Schema+"."+ref.Name]
		return t, ok
	}
	for _, schema := range s.SearchPath {
		if t, ok := s.byName[schema+"."+ref.Name]; ok {
			return t, true
		}
	}
	return nil, false
}

// RelKeyCol is one column participating in a relation key.
//
// The column is the source of truth: a relation is a fact about PostgreSQL, and
// PostgreSQL states it in terms of columns. A mapped Go field, when one exists,
// is an optimisation that lets a loader read the key from an already-scanned
// entity instead of selecting it again.
type RelKeyCol struct {
	// Column is always present.
	Column *PGColumn
	// FieldIdx is the index into the owning entity mapping's Cols, or -1 when
	// the column has no mapped Go field. A value of -1 is legitimate: it selects
	// a loading strategy, it is not an error.
	FieldIdx int
}

// Mapped reports whether the key column has a mapped Go field.
func (k RelKeyCol) Mapped() bool { return k.FieldIdx >= 0 }

// ColMapping records one Go field proved to correspond to one column.
type ColMapping struct {
	Field *GoField
	// Idx is the index of Field within the entity's Fields.
	Idx    int
	Column *PGColumn
}

// RelMapping records one relation field resolved against a foreign key.
//
// KeyCols always name columns on the declaring entity's table and TargetCols
// always name columns on the target's table, whichever side declares the
// constraint. Consumers match entityRow[KeyCols] against targetRow[TargetCols]
// and never branch on FKSide.
type RelMapping struct {
	Field       *GoField
	Cardinality Cardinality
	FKSide      FKSide
	FK          *PGForeignKey

	KeyCols    []RelKeyCol
	TargetCols []RelKeyCol

	// Idx is the index of Field within the entity's Fields.
	Idx int
	// Target is the entity this relation resolves to.
	Target *EntityMapping
}

// EntityMapping records one entity proved to correspond to one table.
type EntityMapping struct {
	Entity *GoEntity
	Table  *PGTable
	Cols   []ColMapping
	Rels   []RelMapping
}

// ColIdx returns the index into Cols of the mapping for column, or -1.
func (m *EntityMapping) ColIdx(col *PGColumn) int {
	for i := range m.Cols {
		if m.Cols[i].Column == col {
			return i
		}
	}
	return -1
}

// Mapping is the whole reconciled correspondence.
type Mapping struct {
	Entities []*EntityMapping
	Schema   *Schema
}

// ConcurrentRefreshIndex names the index REFRESH MATERIALIZED VIEW CONCURRENTLY
// needs, or the empty string when this relation has none that qualifies.
//
// PostgreSQL's requirement is exact and entirely visible in the catalog: at
// least one unique index covering every row, built from plain column names. A
// partial index constrains only the rows it covers; an expression index has no
// column list to match rows by. Neither is a candidate, and offering one would
// mean generating a descriptor that sends a REFRESH the server was always going
// to reject.
//
// When several qualify the lowest name wins, so the answer does not depend on
// the order the catalog happened to return them in.
//
// It lives here because two packages need it and neither may import the other:
// the emitter writes the name into the generated constructor, and the lock puts
// it in the mapping fingerprint so that changing it marks the generated code
// stale. Those two answers must never differ — when they did, the fingerprint
// said nothing had changed while the generated code was wrong.
func (em *EntityMapping) ConcurrentRefreshIndex() string {
	if em == nil || em.Table == nil {
		return ""
	}
	candidates := make([]eligible.Candidate, 0, len(em.Table.Uniques))
	for _, u := range em.Table.Uniques {
		candidates = append(candidates, eligible.Candidate{
			Name: u.Name, Unique: true, Partial: u.Partial,
			Expression: u.Expression, Columns: len(u.Cols),
		})
	}
	return eligible.Choose(candidates)
}

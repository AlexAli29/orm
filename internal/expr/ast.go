// Package expr is the SQL expression tree and its PostgreSQL compiler.
//
// The public API is generic — a Predicate[User] cannot be handed to a query
// over Post, and a column of int32 cannot be compared to a string. None of that
// survives into this package, and none of it needs to: by the time an
// expression reaches the tree, the compiler has already accepted it. Generics
// belong at the boundary, where mistakes are made; the tree underneath is one
// non-generic shape that compiles once.
//
// The package is internal. Nothing here is API, so the node set can grow with
// each milestone without any of it becoming a promise.
package expr

import (
	"errors"
	"fmt"
	"strings"
)

// SourceKind is what a source names.
//
// M3 had one kind and did not need the field. M11 has four, and they differ in
// how they are written into a FROM clause and in nothing else: a column of a
// derived table qualifies against its alias exactly as a column of a table
// does, and scope validation cannot tell them apart because it never asks.
type SourceKind uint8

// The kinds of row source a statement can select from. The zero value is a
// table, which keeps every source built before M11 meaning what it did.
const (
	SourceTable SourceKind = iota
	// SourceDerived is a subquery in a FROM clause. Its alias is required by
	// PostgreSQL and by this package, because a derived table with no name has
	// no way for a column to qualify against it.
	SourceDerived
	// SourceCTE is a reference to a WITH item. The reference and the
	// definition are different things: one WITH item can be referenced twice
	// under two aliases, and those are two sources.
	SourceCTE
)

// Source is one occurrence of a row source in a query.
//
// Identity is the pointer, not the name. Two aliases of one table are two
// sources, and a column belongs to the occurrence it was built from — which is
// what lets the compiler tell "users"."id" from "manager"."id" when both name
// the same table, and what makes scope validation mean anything at all. That
// stays true of the kinds M11 adds: two references to one CTE are two sources,
// and a derived table aliased twice from one query definition is two more.
// Every field here is unexported, and that is a public-API decision rather than
// a style one. Source is reachable from outside this module as orm.Source,
// through a type alias that generated code and extensions both use — so an
// exported field on it would be a permanently public, directly mutable piece of
// the query compiler. Callers hold a *Source, pass it back, and read it through
// methods; nothing outside this package needs more, and nothing outside this
// package had it.
type Source struct {
	// kind decides how the FROM clause writes this source. Everything else
	// about it — identity, scope, how a column qualifies — is the same for
	// every kind.
	kind   SourceKind
	schema string
	table  string
	// alias, when set, is the name the FROM clause binds and the name columns
	// of this occurrence qualify against.
	alias string
	// aliasErr records an alias that cannot be used. As stays chainable, so
	// the mistake has to travel with the source until the compiler — the one
	// place that validates everything — reports it.
	aliasErr error
	// sub is the statement a derived source selects from.
	sub Subquery
	// cTEName is the WITH item a CTE reference resolves to. It is the name
	// PostgreSQL matches, which is not necessarily the name columns qualify
	// against: a reference may be aliased.
	cTEName string
	// outputs are the column names a derived or CTE source provides, in select
	// order. They are what makes a reference to a column this source does not
	// have a mistake the compiler can name, rather than one PostgreSQL reports
	// against SQL the caller did not write.
	outputs []string
	// recursive marks a WITH item whose body refers to itself.
	recursive bool
	// materialized, when set, writes PostgreSQL's MATERIALIZED or NOT
	// MATERIALIZED hint on a WITH item. Leaving it unset leaves the choice to
	// the planner, which is the right default: the hint exists for the cases
	// where the planner's estimate is wrong, and a query builder cannot know
	// that.
	materialized *bool
}

// NewSource returns a source for a schema-qualified table.
func NewSource(schema, table string) *Source {
	return &Source{schema: schema, table: table}
}

// NewDerived returns a derived source: a statement used as a table.
//
// The alias is not optional. PostgreSQL requires one, and without it a column
// of the derived table would have nothing to qualify against.
func NewDerived(alias string, sub Subquery, outputs []string) *Source {
	s := &Source{
		kind:     SourceDerived,
		alias:    alias,
		sub:      sub,
		outputs:  outputs,
		aliasErr: validateAlias(alias),
	}
	if s.aliasErr == nil {
		s.aliasErr = validateOutputs(alias, outputs)
	}
	return s
}

// NewCTERef returns a reference to a WITH item that some statement declares.
//
// It carries no body, which is what a recursive term needs: the self-reference
// exists before the statement that defines it is finished.
func NewCTERef(name string, outputs []string) *Source {
	s := &Source{
		kind:     SourceCTE,
		cTEName:  name,
		outputs:  outputs,
		aliasErr: validateAlias(name),
	}
	if s.aliasErr == nil {
		s.aliasErr = validateOutputs(name, outputs)
	}
	return s
}

// NewCTE returns a WITH item: a named statement, and the source that references
// it.
//
// The definition and the reference are one value because they are one thing —
// the name binds the statement and the name is what a FROM clause writes. An
// alias of it is a second reference to the same item, which is how PostgreSQL
// lets one WITH item be joined to itself.
func NewCTE(name string, body Subquery, outputs []string) *Source {
	s := NewCTERef(name, outputs)
	s.sub = body
	return s
}

// validateOutputs refuses a row source whose columns cannot be told apart.
//
// PostgreSQL compares these names as quoted identifiers, so the comparison is
// exact rather than case-folded: "Count" and "count" are two columns, and only
// two spellings of one name are a collision.
func validateOutputs(name string, outputs []string) error {
	if len(outputs) == 0 {
		return fmt.Errorf("row source %q provides no columns", name)
	}
	seen := make(map[string]bool, len(outputs))
	for _, o := range outputs {
		switch {
		case o == "":
			return fmt.Errorf("row source %q has an unnamed column", name)
		case seen[o]:
			return fmt.Errorf("row source %q provides two columns named %q; a column of a derived table is addressed by name, so the second would be unreachable", name, o)
		}
		seen[o] = true
	}
	return nil
}

// Provides reports whether this source has a column of that name.
//
// A table source answers yes to everything: the catalog decides what columns it
// has, reconciliation already proved the generated descriptors against it, and
// this package has no column list to check against. A derived source or a CTE
// reference does have one, and knowing it is what turns a typo into a message
// naming the columns that do exist.
func (s *Source) Provides(name string) bool {
	if s == nil || len(s.outputs) == 0 {
		return true
	}
	for _, o := range s.outputs {
		if o == name {
			return true
		}
	}
	return false
}

// As returns a new occurrence of the same source under alias. The receiver is
// untouched: an alias is an additional way to name the source, not a change to
// the existing one.
//
// Aliasing a derived source shares its statement rather than copying it, which
// is what makes the same subquery definition usable twice in one statement:
// nothing about a compiled statement is stored in the tree, so two occurrences
// of one definition compile independently.
func (s *Source) As(alias string) *Source {
	out := *s
	out.alias = alias
	out.aliasErr = validateAlias(alias)
	if out.aliasErr == nil {
		out.aliasErr = s.aliasErr
	}
	return &out
}

// Ref is the name columns of this occurrence qualify against.
func (s *Source) Ref() string {
	switch {
	case s.alias != "":
		return s.alias
	case s.kind == SourceCTE:
		return s.cTEName
	default:
		return s.table
	}
}

// String renders the occurrence for a diagnostic.
func (s *Source) String() string {
	if s == nil {
		return "<no source>"
	}
	switch s.kind {
	case SourceDerived:
		return "derived table " + s.alias
	case SourceCTE:
		if s.alias != "" && s.alias != s.cTEName {
			return "CTE " + s.cTEName + " AS " + s.alias
		}
		return "CTE " + s.cTEName
	}
	qualified := s.table
	if s.schema != "" {
		qualified = s.schema + "." + s.table
	}
	if s.alias != "" {
		return qualified + " AS " + s.alias
	}
	return qualified
}

// QuotedName returns the source's table quoted the way the writer quotes it,
// schema-qualified when it has a schema.
//
// It exists so that tooling which has to name a table in a statement of its own
// — a truncate helper, an operational check — uses the writer's own quoting
// rather than a second implementation of it. There is one set of rules for
// turning a name into syntax, and this is how something outside the compiler
// reaches them.
//
// It refuses a name it cannot quote, for the reason the writer does: an
// identifier that is empty or carries a NUL byte would end the identifier early
// and change what the statement means.
func (s *Source) QuotedName() (string, error) {
	if s == nil {
		return "", fmt.Errorf("no source")
	}
	if s.kind != SourceTable {
		return "", fmt.Errorf("%s is not a table", s.String())
	}
	table, err := QuoteIdentifier(s.table)
	if err != nil {
		return "", err
	}
	if s.schema == "" {
		return table, nil
	}
	schema, err := QuoteIdentifier(s.schema)
	if err != nil {
		return "", err
	}
	return schema + "." + table, nil
}

// Reserved returns an occurrence under an alias the compiler chose.
//
// It skips the check As applies, because that check exists to keep callers out
// of this namespace rather than to keep the compiler out of its own. Only the
// relation planner calls it, with names it controls.
func (s *Source) Reserved(alias string) *Source {
	return &Source{schema: s.schema, table: s.table, alias: alias}
}

// ReservedAliasPrefix marks the aliases the compiler generates for itself. A
// caller able to take one of those names could collide with a clause they
// cannot see.
const ReservedAliasPrefix = "_"

func validateAlias(alias string) error {
	switch {
	case alias == "":
		return errors.New("an alias cannot be empty")
	case strings.HasPrefix(alias, ReservedAliasPrefix):
		return fmt.Errorf("alias %q begins with %q, which is reserved for the aliases the compiler generates", alias, ReservedAliasPrefix)
	case strings.ContainsRune(alias, 0):
		return fmt.Errorf("alias %q contains a NUL byte", alias)
	default:
		return nil
	}
}

// Statement is anything that compiles to SQL and its parameters.
type Statement interface {
	Compile() (string, []any, error)
}

// Op is a SQL operator.
type Op uint8

// The operators M2 compiles. The set grows with the milestones; it is not API.
const (
	opInvalid Op = iota
	OpEq
	OpNe
	OpLt
	OpLte
	OpGt
	OpGte
	OpLike
	OpILike
	OpAnd
	OpOr
	OpNot
	OpIsNull
	OpIsNotNull
)

// opSQL is the PostgreSQL spelling of each operator.
var opSQL = map[Op]string{
	OpEq:        "=",
	OpNe:        "<>",
	OpLt:        "<",
	OpLte:       "<=",
	OpGt:        ">",
	OpGte:       ">=",
	OpLike:      "LIKE",
	OpILike:     "ILIKE",
	OpAnd:       "AND",
	OpOr:        "OR",
	OpNot:       "NOT",
	OpIsNull:    "IS NULL",
	OpIsNotNull: "IS NOT NULL",
}

// String returns the operator's PostgreSQL spelling.
func (o Op) String() string {
	if s, ok := opSQL[o]; ok {
		return s
	}
	return "?"
}

// Node is one expression. The interface is closed by an unexported method, so
// the compiler's type switch is exhaustive by construction.
type Node interface {
	node()
}

// Column is a reference to a column of a source.
type Column struct {
	Source *Source
	Name   string
}

// Arg is a value. Values are always parameters: nothing in this package can
// place a caller's value into the SQL text.
type Arg struct {
	Value any
}

// Bool is a constant TRUE or FALSE.
//
// It exists so that the degenerate cases have a representation rather than a
// special case: an And of nothing is TRUE, an Or of nothing is FALSE, and an IN
// over no values is FALSE. Each is the identity or annihilator its operator
// deserves, and each compiles to legal SQL.
type Bool struct {
	Value bool
}

// Binary is an infix comparison.
type Binary struct {
	Op          Op
	Left, Right Node
}

// Unary is a prefix or postfix operator.
type Unary struct {
	Op Op
	X  Node
}

// Group is a run of nodes joined by one boolean operator.
type Group struct {
	Op    Op
	Items []Node
}

// In is a membership test.
type In struct {
	X      Node
	Values []Node
}

// Between is an inclusive range test.
type Between struct {
	X      Node
	Lo, Hi Node
}

// Raw is a fragment of SQL the caller wrote, with its own parameters.
//
// It is the escape hatch, and it is deliberately narrow: the fragment's own
// $1, $2 placeholders are renumbered into the surrounding statement and its
// arguments are appended to the surrounding parameter list, so a value the
// caller supplied is still never part of the text. What the caller gives up is
// the type checking, not the parameterisation.
type Raw struct {
	SQL  string
	Args []any
}

func (Raw) node() {}

// Fail is a mistake made while building an expression.
//
// Most of this package's mistakes are recorded in a builder, which is where
// several of them can be collected and reported together. An expression has no
// builder — it is a value returned from a constructor, and a constructor that
// returned an error would not compose — so the mistake travels in the tree
// until the compiler, which has an error return, reaches it. It never compiles
// to anything: there is no SQL for which this node is the right answer.
type Fail struct {
	Err error
}

func (Fail) node() {}

// Null is the SQL NULL literal.
//
// It is a node rather than a parameter carrying nil because assigning NULL and
// binding a nil value are different statements to write, and because NULL is
// the one value that never needs to be sent.
type Null struct{}

// Excluded refers to a column of the row an INSERT could not add, which is what
// ON CONFLICT DO UPDATE reads to decide the new value.
type Excluded struct {
	Name string
}

func (Null) node()     {}
func (Excluded) node() {}

// Exists is a correlated subquery testing whether any row matches.
//
// It is how a relation filters root rows without joining anything into them:
// EXISTS answers a question about the related table and returns one boolean per
// root row, so the root's cardinality is untouched and no deduplication is
// needed. A join would return one root row per match, which is a different
// query wearing the same words.
//
// Sub selects the constant 1. Nothing reads a value from it, and asking for
// columns would make the server fetch data no part of the statement uses.
type Exists struct {
	// Not renders NOT EXISTS, which is how a relation asks for the absence of
	// a match. The alternative — an outer join with IS NULL — is longer, easier
	// to get wrong, and changes the cardinality on the way.
	Not bool
	Sub *Select
}

func (Exists) node() {}

// Assignment sets one column to one value.
type Assignment struct {
	Column Column
	Value  Node
}

func (Column) node()  {}
func (Arg) node()     {}
func (Bool) node()    {}
func (Binary) node()  {}
func (Unary) node()   {}
func (Group) node()   {}
func (In) node()      {}
func (Between) node() {}

// IsTrue reports whether n is the constant TRUE, which a WHERE clause may drop.
func IsTrue(n Node) bool {
	b, ok := n.(Bool)
	return ok && b.Value
}

// The small API the root package needs now that a Source's fields are its own.
//
// Source is reachable from outside this module as orm.Source, so an exported
// field on it is a permanently public, directly mutable piece of the query
// compiler. These four are what the root package actually did with those
// fields, written as operations instead: what a caller can do to a Source is
// now a list somebody can read.

// FailedCTE returns a CTE source that carries an error instead of a definition.
//
// The error travels with the source rather than being returned, because the
// builder stays chainable: the mistake is reported by the compiler, which is
// the one place that sees the whole statement.
func FailedCTE(name string, recursive bool, err error) *Source {
	return &Source{kind: SourceCTE, cTEName: name, recursive: recursive, aliasErr: err}
}

// Err returns the error this source carries, if it was built from something
// that could not be used.
func (s *Source) Err() error {
	if s == nil {
		return nil
	}
	return s.aliasErr
}

// SetMaterialized records whether PostgreSQL should be asked to materialise a
// CTE. A nil Source is ignored, so an option applied to a failed CTE is not a
// second failure.
//
// It is a function rather than a method, and deliberately.
//
// Source is aliased into the root package as orm.Source, so every exported
// method on it is something any caller holding a source descriptor can do —
// including to the package-level descriptor generated code declares once and
// shares with every query in the process. As a method this was a public,
// unsynchronised writer for the compiler's own state: two goroutines composing
// queries while a third called it is a data race the race detector reports, and
// nothing about the API said so.
//
// A package-level function in an internal package is reachable by the packages
// that legitimately build statements and by nobody else. The construction path
// is unchanged; what changes is that the door is no longer in the public wall.
func SetMaterialized(s *Source, want bool) {
	if s == nil {
		return
	}
	s.materialized = &want
}

// IsDerived reports whether this source is a sub-SELECT, which is the only kind
// LATERAL may be attached to.
func (s *Source) IsDerived() bool { return s != nil && s.kind == SourceDerived }

// IsCTE reports whether this source is a WITH item or a reference to one.
func (s *Source) IsCTE() bool { return s != nil && s.kind == SourceCTE }

// HasDefinition reports whether a CTE source carries the statement it names.
//
// A reference to a CTE declared elsewhere has a name and no statement, which is
// the difference between "WITH x AS (...)" and a later mention of x.
func (s *Source) HasDefinition() bool { return s != nil && s.sub != nil }

// Name returns the WITH item's name, which is what a diagnostic quotes.
func (s *Source) Name() string {
	if s == nil {
		return ""
	}
	return s.cTEName
}

// SetRecursive marks a CTE as recursive, which changes the WITH keyword the
// whole clause is written with.
//
// It is a function rather than a method for the reason given on
// [SetMaterialized]: as a method it was a public mutator on the compiler's
// state, and marking a plain table source recursive is not a thing any caller
// should be able to attempt.
func SetRecursive(s *Source) {
	if s != nil {
		s.recursive = true
	}
}

// IsTable reports whether this source is a real table, which is the only kind
// that has rows of its own to lock.
func (s *Source) IsTable() bool { return s != nil && s.kind == SourceTable }

// Relation returns the schema and table this source reads, empty for a source
// that is not a table.
func (s *Source) Relation() (schema, table string) {
	if s == nil {
		return "", ""
	}
	return s.schema, s.table
}

// FailedDerived returns a derived source carrying an error instead of a
// sub-SELECT.
func FailedDerived(alias string, err error) *Source {
	return &Source{kind: SourceDerived, alias: alias, aliasErr: err}
}

// AliasName returns the alias bound to this occurrence, empty when none is.
func (s *Source) AliasName() string {
	if s == nil {
		return ""
	}
	return s.alias
}

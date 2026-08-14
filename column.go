package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Source is one occurrence of a row source in a query.
//
// It began as the table a generated descriptor reads from, and it is now the
// one model for every kind of thing a FROM clause can name: a table, an alias
// of one, a derived table, a reference to a WITH item. They differ in how they
// are written and in nothing else — a column of a derived table qualifies
// against its alias exactly as a column of a table does, and scope validation
// cannot tell them apart because it never asks.
//
// Identity is the pointer rather than the name. Two aliases of one table are
// two sources, two references to one CTE are two sources, and a column belongs
// to the occurrence it was built from — which is what lets the compiler tell
// "users"."id" from "manager"."id" and what makes scope validation mean
// anything at all.
//
// It is an alias rather than a wrapper because the expression tree and the
// public API refer to the same occurrence: introducing two types would mean
// copying between them on every query for no gain. Build one with [NewSource],
// a table descriptor's As, [Sub], [CTE], [RecursiveCTE] or [WritingCTE]; the
// fields are the compiler's and nothing outside it needs to read them.
type Source = expr.Source

// NewSource returns a source for a schema-qualified table. Generated code calls
// it; there is no reason to call it by hand.
func NewSource(schema, table string) *Source { return expr.NewSource(schema, table) }

// The column descriptor types form a capability lattice. Each adds the
// operations PostgreSQL actually supports for the kind of column it describes,
// and adds nothing else — which is the point. A column that cannot be NULL has
// no IsNull method to call, and a jsonb column has no Gt, because the generator
// assigns capabilities from the PostgreSQL type rather than from whatever Go
// type happens to be on the other side.
//
//	Col          equality, membership, ordering direction
//	OrdCol       + magnitude comparison and ranges
//	TextCol      + pattern matching
//	NullCol      Col + null tests
//	NullOrdCol   OrdCol + null tests
//	NullTextCol  TextCol + null tests
//
// E is the entity the column belongs to. It appears in every predicate the
// column produces, so a predicate over one entity cannot reach a query over
// another: the mistake is a compile error, not a runtime check.
type Col[E, V any] struct {
	col expr.Column
}

// NewCol returns a base column descriptor. Generated code calls it.
func NewCol[E, V any](src *Source, name string) Col[E, V] {
	return Col[E, V]{col: expr.Column{Source: src, Name: name}}
}

// Column returns the column's name.
func (c Col[E, V]) Column() string { return c.col.Name }

// Source returns the table occurrence this column belongs to. Two descriptors
// for the same column of the same table differ here when one came from [As].
func (c Col[E, V]) Source() *Source { return c.col.Source }

// colRef and colTyped make a descriptor satisfy [ColumnOf]. They are
// unexported so the interface stays closed: only descriptors from this package
// can be compared to one another.
func (c Col[E, V]) colRef() expr.Column { return c.col }
func (c Col[E, V]) colTyped(E, V)       {}

// ColumnOf is any descriptor for a column of entity E holding values of type V.
//
// The type parameters appear in colTyped's signature rather than going unused,
// which is what makes ColumnOf[User, int64] a different interface from
// ColumnOf[Post, int64] instead of the same one twice. That is the whole reason
// it exists: it lets EqCol accept any capability — a plain column, an ordered
// one, a nullable one — while still refusing a column of another entity or
// another value type.
type ColumnOf[E, V any] interface {
	colRef() expr.Column
	colTyped(E, V)
}

// EqCol builds column = other, comparing two columns rather than a column and a
// value.
//
// Both must belong to the same entity and hold the same type, which the
// compiler enforces. It is the operation an alias exists for:
//
//	manager := Users.As("manager")
//	manager.ID.EqCol(Users.ManagerID)
//
// M3 offers equality only. The ordered and pattern-matching forms are easy to
// add and would widen the public surface before anything needs them.
func (c Col[E, V]) EqCol(other ColumnOf[E, V]) Predicate[E] {
	return Predicate[E]{node: expr.Binary{Op: expr.OpEq, Left: c.col, Right: other.colRef()}}
}

// Eq builds column = value.
func (c Col[E, V]) Eq(v V) Predicate[E] { return c.binary(expr.OpEq, v) }

// Ne builds column <> value.
func (c Col[E, V]) Ne(v V) Predicate[E] { return c.binary(expr.OpNe, v) }

// In builds column IN (values...).
//
// In over no values is FALSE. PostgreSQL has no syntax for an empty IN list,
// and a caller who passed an empty slice meant that nothing matches — so that
// is what it compiles to, rather than a syntax error at the database.
func (c Col[E, V]) In(vs ...V) Predicate[E] {
	nodes := make([]expr.Node, 0, len(vs))
	for _, v := range vs {
		nodes = append(nodes, expr.Arg{Value: v})
	}
	return Predicate[E]{node: expr.In{X: c.col, Values: nodes}}
}

// Asc orders by the column ascending.
func (c Col[E, V]) Asc() Order[E] { return Order[E]{order: expr.Order{Column: c.col}} }

// Desc orders by the column descending.
func (c Col[E, V]) Desc() Order[E] { return Order[E]{order: expr.Order{Column: c.col, Desc: true}} }

func (c Col[E, V]) binary(op expr.Op, v V) Predicate[E] {
	return Predicate[E]{node: expr.Binary{Op: op, Left: c.col, Right: expr.Arg{Value: v}}}
}

// OrdCol describes a column PostgreSQL can order and compare by magnitude.
//
// V is deliberately unconstrained. SQL ordering is the column type's business,
// not Go's: a timestamptz orders in PostgreSQL whether or not time.Time
// satisfies cmp.Ordered, and requiring it would exclude exactly the types most
// worth comparing.
type OrdCol[E, V any] struct {
	Col[E, V]
}

// NewOrdCol returns an ordered column descriptor. Generated code calls it.
func NewOrdCol[E, V any](src *Source, name string) OrdCol[E, V] {
	return OrdCol[E, V]{Col: NewCol[E, V](src, name)}
}

// Gt builds column > value.
func (c OrdCol[E, V]) Gt(v V) Predicate[E] { return c.binary(expr.OpGt, v) }

// Gte builds column >= value.
func (c OrdCol[E, V]) Gte(v V) Predicate[E] { return c.binary(expr.OpGte, v) }

// Lt builds column < value.
func (c OrdCol[E, V]) Lt(v V) Predicate[E] { return c.binary(expr.OpLt, v) }

// Lte builds column <= value.
func (c OrdCol[E, V]) Lte(v V) Predicate[E] { return c.binary(expr.OpLte, v) }

// Between builds column BETWEEN lo AND hi, which is inclusive at both ends.
func (c OrdCol[E, V]) Between(lo, hi V) Predicate[E] {
	return Predicate[E]{node: expr.Between{
		X:  c.col,
		Lo: expr.Arg{Value: lo},
		Hi: expr.Arg{Value: hi},
	}}
}

// TextCol describes a character column: ordered, and matchable by pattern.
type TextCol[E any] struct {
	OrdCol[E, string]
}

// NewTextCol returns a text column descriptor. Generated code calls it.
func NewTextCol[E any](src *Source, name string) TextCol[E] {
	return TextCol[E]{OrdCol: NewOrdCol[E, string](src, name)}
}

// Like builds column LIKE pattern, which is case sensitive.
func (c TextCol[E]) Like(pattern string) Predicate[E] { return c.binary(expr.OpLike, pattern) }

// ILike builds column ILIKE pattern, which is case insensitive.
func (c TextCol[E]) ILike(pattern string) Predicate[E] { return c.binary(expr.OpILike, pattern) }

// NullCol describes a nullable column.
//
// V is the value type, not the Go field type: a nullable text column is *string
// in the struct but compares against a string, because SQL compares values and
// NULL is tested for rather than compared to.
type NullCol[E, V any] struct {
	Col[E, V]
}

// NewNullCol returns a nullable base column descriptor. Generated code calls it.
func NewNullCol[E, V any](src *Source, name string) NullCol[E, V] {
	return NullCol[E, V]{Col: NewCol[E, V](src, name)}
}

// IsNull builds column IS NULL.
func (c NullCol[E, V]) IsNull() Predicate[E] { return isNull[E](c.col, true) }

// IsNotNull builds column IS NOT NULL.
func (c NullCol[E, V]) IsNotNull() Predicate[E] { return isNull[E](c.col, false) }

// NullOrdCol describes a nullable column PostgreSQL can order.
type NullOrdCol[E, V any] struct {
	OrdCol[E, V]
}

// NewNullOrdCol returns a nullable ordered column descriptor. Generated code
// calls it.
func NewNullOrdCol[E, V any](src *Source, name string) NullOrdCol[E, V] {
	return NullOrdCol[E, V]{OrdCol: NewOrdCol[E, V](src, name)}
}

// IsNull builds column IS NULL.
func (c NullOrdCol[E, V]) IsNull() Predicate[E] { return isNull[E](c.col, true) }

// IsNotNull builds column IS NOT NULL.
func (c NullOrdCol[E, V]) IsNotNull() Predicate[E] { return isNull[E](c.col, false) }

// NullTextCol describes a nullable character column.
type NullTextCol[E any] struct {
	TextCol[E]
}

// NewNullTextCol returns a nullable text column descriptor. Generated code
// calls it.
func NewNullTextCol[E any](src *Source, name string) NullTextCol[E] {
	return NullTextCol[E]{TextCol: NewTextCol[E](src, name)}
}

// IsNull builds column IS NULL.
func (c NullTextCol[E]) IsNull() Predicate[E] { return isNull[E](c.col, true) }

// IsNotNull builds column IS NOT NULL.
func (c NullTextCol[E]) IsNotNull() Predicate[E] { return isNull[E](c.col, false) }

func isNull[E any](c expr.Column, want bool) Predicate[E] {
	op := expr.OpIsNotNull
	if want {
		op = expr.OpIsNull
	}
	return Predicate[E]{node: expr.Unary{Op: op, X: c}}
}

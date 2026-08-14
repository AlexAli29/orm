package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Typed value expressions.
//
// A predicate compiles to a boolean and answers "which rows". A value
// expression compiles to a value and answers "what is returned" — a projected
// column, an aggregate, the right-hand side of an assignment. They are the same
// tree underneath, and this is the typed handle on the value-shaped part of it.
//
// Two things travel with the tree that the tree does not know:
//
//	E  the entity whose sources the expression may name
//	T  the Go type a row scans it into
//
// E is what stops a column of Post from reaching a query over User. T is where
// nullability lives: a nullable column's value expression is Value[E, *string],
// not Value[E, string], so binding it to a destination that cannot hold NULL is
// a compile error rather than a zero value nobody asked for.

// Value is a typed SQL expression over entity E producing a Go value of type T.
//
// It is a value type with no exported fields: everything that builds one is a
// constructor in this package, so the set of expressions is exactly the set the
// compiler can render.
type Value[E, T any] struct {
	node  expr.Node
	alias string
	// nullable records that T can hold a SQL NULL.
	//
	// The tree below does not know what a Go type is, and the compiler needs
	// the fact: it is what tells a recursive CTE's convergence check that a
	// term can produce NULL, and what tells the outer-join check that a
	// destination can absorb one. Every constructor that knows the answer
	// fills it in, and the two that cannot — RawValue, where the caller states
	// the type, and a bare expression built from a non-nullable operand —
	// leave it false, which is the conservative direction for both readers.
	nullable bool
}

// Selectable is anything that can appear in a select list, a GROUP BY, or the
// right-hand side of an assignment, producing T for entity E.
//
// The type parameters appear in a method signature rather than only in the type
// name, which is what makes Selectable[User, int64] a different interface from
// Selectable[Post, int64] instead of the same one twice. Without that, every
// projection would accept every column of every entity.
//
// It is closed by unexported methods, so only this package's expressions
// satisfy it.
type Selectable[E, T any] interface {
	selectItem() expr.SelectItem
	selectTyped(E, T)
}

func (v Value[E, T]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: v.node, Alias: v.alias, Nullable: v.nullable}
}
func (v Value[E, T]) selectTyped(E, T) {}

// As names the value in the result.
//
// This is a result alias and has nothing to do with a table alias: it names a
// column of the returned rowset, where Users.As("u") names an occurrence of a
// table. Confusing them is easy enough that they are kept apart in the tree as
// well as here.
//
// It returns a new value; the receiver is untouched, so aliasing a generated
// descriptor cannot change what that descriptor means anywhere else.
func (v Value[E, T]) As(alias string) Value[E, T] {
	out := v
	out.alias = alias
	return out
}

// Nullable widens an expression to its nullable form.
//
// A non-nullable expression is a legal thing to store in a nullable column, but
// the types differ — Value[E, string] against Value[E, *string] — because
// nullability is part of the result type rather than a property checked later.
//
// This is the one direction that is always sound, so it is the one offered:
// there is no narrowing counterpart, because assuming an expression cannot
// produce NULL is exactly the assumption this design refuses to make.
//
// It is a function rather than a method because a method returning
// Value[E, *T] would instantiate itself, which Go refuses.
func Nullable[E, T any](v Selectable[E, T]) Value[E, *T] {
	it := v.selectItem()
	return Value[E, *T]{node: it.Node, alias: it.Alias, nullable: true}
}

// Grouping is anything a GROUP BY may name.
//
// It is satisfied by whatever produces a value for entity E, whatever that
// value's Go type: grouping does not read the value, so the result type is not
// part of the question. E still is, because grouping by a column of another
// table is the same mistake it is anywhere else.
// E appears in a method signature rather than only in the type name. Without
// that it would be phantom, Grouping[User] and Grouping[Post] would be the same
// interface, and a query over one entity would happily group by a column of
// another.
type Grouping[E any] interface {
	groupNode() expr.Node
	groupTyped(E)
}

func (v Value[E, T]) groupNode() expr.Node { return v.node }
func (v Value[E, T]) groupTyped(E)         {}
func (c Col[E, V]) groupNode() expr.Node   { return c.col }
func (c Col[E, V]) groupTyped(E)           {}

// Expr builds a typed value from SQL the caller wrote.
//
// It is the escape hatch for an expression this package cannot yet spell, and
// it is deliberately explicit in two ways. The SQL is written by the caller, so
// nothing about it is checked beyond its placeholders — its own $1, $2 are
// renumbered into the surrounding statement and its arguments join the
// parameter list, so values are still never text. And the result type is stated
// rather than inferred: PostgreSQL decides what an arbitrary expression returns
// and whether it can be NULL, and guessing either from the text would be
// guessing about NULL.
//
//	orm.RawValue[User, string](`lower("users"."email")`)
//	orm.RawValue[User, *string](`nullif("users"."bio", '')`)
//
// Choose the nullable form unless the expression genuinely cannot produce NULL.
// This is a trust boundary: nothing checks the claim, so a non-nullable
// declaration over an expression that returns NULL fails as a scan error, and a
// wrong result type fails the same way. Both are clean failures rather than
// wrong values, which is the most this package can promise about SQL it did not
// write.
func RawValue[E, T any](sql string, args ...any) Value[E, T] {
	return Value[E, T]{node: expr.Raw{SQL: sql, Args: args}}
}

// columnValue builds the typed handle for a column descriptor.
//
// nullable is the descriptor's own answer rather than something inferred from
// T: a nullable column's handle is Value[E, *V] and can hold a NULL, and a
// non-nullable one's cannot.
func columnValue[E, T any](c expr.Column, nullable bool) Value[E, T] {
	return Value[E, T]{node: c, nullable: nullable}
}

// Selecting a column.
//
// A non-nullable descriptor selects as its value type; a nullable one selects
// as a pointer to it, because that is the only Go shape that can tell SQL NULL
// from the type's zero value. Each nullable descriptor shadows the promoted
// method rather than adding a second one, so exactly one of the two is in its
// method set — which is what makes the nullable form the only thing that
// compiles for a nullable column.

func (c Col[E, V]) selectItem() expr.SelectItem { return expr.SelectItem{Node: c.col} }
func (c Col[E, V]) selectTyped(E, V)            {}

// As names the column in the result.
func (c Col[E, V]) As(alias string) Value[E, V] {
	return columnValue[E, V](c.col, false).As(alias)
}

// Value returns the column as an expression, which is how one column is
// assigned to another or composed into arithmetic.
func (c Col[E, V]) Value() Value[E, V] { return columnValue[E, V](c.col, false) }

func (c NullCol[E, V]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: c.col, Nullable: true}
}
func (c NullCol[E, V]) selectTyped(E, *V) {}

// As names the column in the result. A nullable column produces a pointer, so
// that SQL NULL stays distinguishable from the type's zero value.
func (c NullCol[E, V]) As(alias string) Value[E, *V] {
	return columnValue[E, *V](c.col, true).As(alias)
}

// Value returns the column as a nullable expression.
//
// It shadows the promoted non-nullable form rather than adding to it. Without
// that, reading a nullable column through Value would produce an expression
// claiming it cannot be NULL — and the claim would be believed by every
// destination and every assignment downstream.
func (c NullCol[E, V]) Value() Value[E, *V] { return columnValue[E, *V](c.col, true) }

func (c NullOrdCol[E, V]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: c.col, Nullable: true}
}
func (c NullOrdCol[E, V]) selectTyped(E, *V) {}

// As names the column in the result.
func (c NullOrdCol[E, V]) As(alias string) Value[E, *V] {
	return columnValue[E, *V](c.col, true).As(alias)
}

// Value returns the column as a nullable expression.
func (c NullOrdCol[E, V]) Value() Value[E, *V] { return columnValue[E, *V](c.col, true) }

// The arithmetic of a nullable column is nullable, because SQL arithmetic
// propagates NULL: NULL + 1 is NULL, not 1. Each of these shadows the promoted
// non-nullable form for that reason.

// Add builds column + value, which is NULL when the column is.
func (c NullOrdCol[E, V]) Add(v V) Value[E, *V] { return c.nullArith(expr.OpAdd, expr.Arg{Value: v}) }

// Sub builds column - value, which is NULL when the column is.
func (c NullOrdCol[E, V]) Sub(v V) Value[E, *V] { return c.nullArith(expr.OpSub, expr.Arg{Value: v}) }

// Mul builds column * value, which is NULL when the column is.
func (c NullOrdCol[E, V]) Mul(v V) Value[E, *V] { return c.nullArith(expr.OpMul, expr.Arg{Value: v}) }

// Div builds column / value, which is NULL when the column is.
func (c NullOrdCol[E, V]) Div(v V) Value[E, *V] { return c.nullArith(expr.OpDiv, expr.Arg{Value: v}) }

// AddCol builds column + other, which is NULL when either side is.
func (c NullOrdCol[E, V]) AddCol(other Selectable[E, V]) Value[E, *V] {
	return c.nullArith(expr.OpAdd, other.selectItem().Node)
}

// SubCol builds column - other, which is NULL when either side is.
func (c NullOrdCol[E, V]) SubCol(other Selectable[E, V]) Value[E, *V] {
	return c.nullArith(expr.OpSub, other.selectItem().Node)
}

func (c NullOrdCol[E, V]) nullArith(op expr.ArithOp, right expr.Node) Value[E, *V] {
	return Value[E, *V]{node: expr.Arith{Op: op, Left: c.col, Right: right}, nullable: true}
}

func (c NullTextCol[E]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: c.col, Nullable: true}
}
func (c NullTextCol[E]) selectTyped(E, *string) {}

// As names the column in the result.
func (c NullTextCol[E]) As(alias string) Value[E, *string] {
	return columnValue[E, *string](c.col, true).As(alias)
}

// Value returns the column as a nullable expression.
func (c NullTextCol[E]) Value() Value[E, *string] { return columnValue[E, *string](c.col, true) }

// The arithmetic promoted from OrdCol[E, string] is shadowed for the same
// reason it is on the other nullable descriptors: over a nullable column the
// result is nullable. PostgreSQL has no + for text, so these exist to carry the
// type rather than to be used — the server refuses them either way, and it
// refuses them the same way it refuses the non-nullable form.

// Add builds column + value, which is NULL when the column is.
func (c NullTextCol[E]) Add(v string) Value[E, *string] { return c.nullTextArith(expr.OpAdd, v) }

// Sub builds column - value, which is NULL when the column is.
func (c NullTextCol[E]) Sub(v string) Value[E, *string] { return c.nullTextArith(expr.OpSub, v) }

// Mul builds column * value, which is NULL when the column is.
func (c NullTextCol[E]) Mul(v string) Value[E, *string] { return c.nullTextArith(expr.OpMul, v) }

// Div builds column / value, which is NULL when the column is.
func (c NullTextCol[E]) Div(v string) Value[E, *string] { return c.nullTextArith(expr.OpDiv, v) }

// AddCol builds column + other, which is NULL when either side is.
func (c NullTextCol[E]) AddCol(other Selectable[E, string]) Value[E, *string] {
	return Value[E, *string]{node: expr.Arith{
		Op: expr.OpAdd, Left: c.col, Right: other.selectItem().Node,
	}, nullable: true}
}

// SubCol builds column - other, which is NULL when either side is.
func (c NullTextCol[E]) SubCol(other Selectable[E, string]) Value[E, *string] {
	return Value[E, *string]{node: expr.Arith{
		Op: expr.OpSub, Left: c.col, Right: other.selectItem().Node,
	}, nullable: true}
}

func (c NullTextCol[E]) nullTextArith(op expr.ArithOp, v string) Value[E, *string] {
	return Value[E, *string]{node: expr.Arith{Op: op, Left: c.col, Right: expr.Arg{Value: v}}, nullable: true}
}

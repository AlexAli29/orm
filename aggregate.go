package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Aggregates.
//
// An aggregate is a value expression like any other: it is selected, aliased,
// compared and scanned through the same machinery a column is. What is special
// about it is its type, and the type is PostgreSQL's rather than Go's.
//
// Two rules decide every signature here, and both are PostgreSQL's:
//
//   - An aggregate over no rows is NULL. Not zero, not the type's identity —
//     NULL. count is the single exception, because counting nothing is nought.
//     So every aggregate but count produces a pointer, and a query that
//     matched nothing returns nil rather than a number nobody computed.
//
//   - An aggregate promotes its input type. sum(int4) is bigint, sum(int8) is
//     numeric, avg of any integer is numeric. Go's arithmetic has nothing to
//     say about that, so the constructors are written per input type rather
//     than being one generic function over a numeric constraint — a signature
//     that admitted every numeric would have to lie about at least one of them.

// Aggregatable is a column an aggregate can be taken over.
//
// It is satisfied by column descriptors, which is deliberate: an aggregate
// applies to a column of a table, and letting it apply to an arbitrary
// expression would mean deciding the result type of an expression nobody typed.
type Aggregatable[E, V any] interface {
	colRef() expr.Column
	colTyped(E, V)
}

// aggregate builds the typed handle for an aggregate call.
func aggregate[E, T any](fn string, args []expr.Node, opts aggOpts) Agg[E, T] {
	return Agg[E, T]{nullable: opts.nullable, agg: expr.Aggregate{
		Func:     fn,
		Args:     args,
		Star:     opts.star,
		Distinct: opts.distinct,
		Filter:   opts.filter,
	}}
}

type aggOpts struct {
	star     bool
	distinct bool
	filter   expr.Node
	// nullable records that the result type can hold a NULL, which for an
	// aggregate is decided by the function rather than by its input: every one
	// but count is NULL over an empty group. The compiler needs the fact and
	// cannot derive it, so each constructor states it.
	nullable bool
}

// Agg is an aggregate expression over entity E producing a Go value of type T.
//
// It is selectable wherever a value is, and it carries the comparisons a HAVING
// clause needs. T already accounts for PostgreSQL's promotion and for the NULL
// an empty group produces, so nothing downstream has to reason about either.
type Agg[E, T any] struct {
	agg      expr.Aggregate
	alias    string
	nullable bool
}

func (a Agg[E, T]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: a.agg, Alias: a.alias, Nullable: a.nullable}
}
func (a Agg[E, T]) selectTyped(E, T) {}

// As names the aggregate in the result. It does not change its type.
func (a Agg[E, T]) As(alias string) Agg[E, T] {
	out := a
	out.alias = alias
	return out
}

// Distinct aggregates over the distinct values of the argument.
//
// This is DISTINCT inside the call — count(DISTINCT x) — which is a different
// thing from [SelectQuery.Distinct], where DISTINCT applies to the whole result
// row.
func (a Agg[E, T]) Distinct() Agg[E, T] {
	out := a
	out.agg.Distinct = true
	return out
}

// Filter restricts the rows this aggregate sees, and only this one.
//
//	orm.Count[Order]().Filter(Orders.Status.Eq(StatusPaid))
//
// It compiles to PostgreSQL's FILTER (WHERE ...), so the statement's own WHERE
// is untouched: two aggregates in one select list can count different subsets
// of the same rows, which is the whole reason the clause exists.
func (a Agg[E, T]) Filter(ps ...Predicate[E]) Agg[E, T] {
	out := a
	if p := And(ps...); !expr.IsTrue(p.node) {
		out.agg.Filter = p.node
	}
	return out
}

// Comparisons.
//
// These are what a HAVING clause is built from. They produce an ordinary
// Predicate[E], because a predicate has never been more than a boolean node
// tagged with the entity whose sources it may name — which is as true of a
// condition over a group as of one over a row. What decides where such a
// predicate may go is the clause, and [SelectQuery.Where] refuses an aggregate
// for exactly that reason.
//
// The comparison value is T, so it accounts for promotion and nullability the
// same way the result does: comparing a sum of bigint means comparing a
// *pgtype.Numeric-shaped value, not an int64 that would silently truncate.

// Eq builds aggregate = value.
func (a Agg[E, T]) Eq(v T) Predicate[E] { return a.cmp(expr.OpEq, v) }

// Ne builds aggregate <> value.
func (a Agg[E, T]) Ne(v T) Predicate[E] { return a.cmp(expr.OpNe, v) }

// Gt builds aggregate > value.
func (a Agg[E, T]) Gt(v T) Predicate[E] { return a.cmp(expr.OpGt, v) }

// Gte builds aggregate >= value.
func (a Agg[E, T]) Gte(v T) Predicate[E] { return a.cmp(expr.OpGte, v) }

// Lt builds aggregate < value.
func (a Agg[E, T]) Lt(v T) Predicate[E] { return a.cmp(expr.OpLt, v) }

// Lte builds aggregate <= value.
func (a Agg[E, T]) Lte(v T) Predicate[E] { return a.cmp(expr.OpLte, v) }

// IsNull builds aggregate IS NULL, which is how an empty group is tested for.
func (a Agg[E, T]) IsNull() Predicate[E] {
	return Predicate[E]{node: expr.Unary{Op: expr.OpIsNull, X: a.agg}}
}

// IsNotNull builds aggregate IS NOT NULL.
func (a Agg[E, T]) IsNotNull() Predicate[E] {
	return Predicate[E]{node: expr.Unary{Op: expr.OpIsNotNull, X: a.agg}}
}

func (a Agg[E, T]) cmp(op expr.Op, v T) Predicate[E] {
	return Predicate[E]{node: expr.Binary{Op: op, Left: a.agg, Right: expr.Arg{Value: v}}}
}

// Count counts the rows of a group.
//
//	orm.Count[User]()
//
// PostgreSQL's count returns bigint and never NULL — counting nothing is zero —
// so this is the one aggregate whose result is not a pointer. The entity has to
// be named because there is no argument to infer it from.
//
// This is not [Query.Count]. That one asks how many rows a query returns and is
// answered by wrapping the statement; this is an expression selected alongside
// others, and in a grouped query it counts each group.
func Count[E any]() Agg[E, int64] {
	return aggregate[E, int64]("count", nil, aggOpts{star: true})
}

// CountOf counts the rows of a group where the column is not NULL.
//
//	orm.CountOf(Posts.AuthorID)
//
// That is what count(expr) means in SQL, and it is why it differs from
// [Count]: a column with NULLs counts fewer rows than the group has.
func CountOf[E, V any](c Aggregatable[E, V]) Agg[E, int64] {
	return aggregate[E, int64]("count", []expr.Node{c.colRef()}, aggOpts{})
}

// Min is the smallest value in a group, or nil when the group has none.
//
// The result keeps the column's own type, because PostgreSQL's min does. It is
// a pointer because an aggregate over no rows is NULL — a group with rows in it
// always has a minimum, but the type cannot tell the two cases apart, and a
// non-nullable result would be a promise this package cannot keep for a query
// that matches nothing.
func Min[E, V any](c Aggregatable[E, V]) Agg[E, *V] {
	return aggregate[E, *V]("min", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// Max is the largest value in a group, or nil when the group has none.
func Max[E, V any](c Aggregatable[E, V]) Agg[E, *V] {
	return aggregate[E, *V]("max", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// The sums.
//
// PostgreSQL promotes, and the promotion is not the input type:
//
//	sum(smallint)         -> bigint
//	sum(integer)          -> bigint
//	sum(bigint)           -> numeric
//	sum(numeric)          -> numeric
//	sum(real)             -> real
//	sum(double precision) -> double precision
//
// So there is one constructor per input type rather than one generic function.
// A single Sum[V] would have to claim sum(int8) is int8, which overflows
// silently at exactly the scale a sum is worth taking.
//
// The numeric results are typed as the caller's own numeric mapping, because
// this project refuses to map numeric to float64: the mapping is configured,
// and an aggregate is not a reason to abandon it. N is that Go type, and it is
// named at the call site because only the project knows it.

// SumInt16 sums a smallint column. PostgreSQL returns bigint.
func SumInt16[E any](c Aggregatable[E, int16]) Agg[E, *int64] {
	return aggregate[E, *int64]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// SumInt32 sums an integer column. PostgreSQL returns bigint.
func SumInt32[E any](c Aggregatable[E, int32]) Agg[E, *int64] {
	return aggregate[E, *int64]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// SumInt64 sums a bigint column. PostgreSQL returns numeric, so N is the Go
// type this project maps numeric to:
//
//	orm.SumInt64[User, decimal.Decimal](Users.Visits)
//
// It is not int64. A sum of bigints overflows bigint, which is why PostgreSQL
// widens it, and returning int64 here would put the overflow back.
func SumInt64[E, N any](c Aggregatable[E, int64]) Agg[E, *N] {
	return aggregate[E, *N]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// SumFloat32 sums a real column. PostgreSQL returns real.
func SumFloat32[E any](c Aggregatable[E, float32]) Agg[E, *float32] {
	return aggregate[E, *float32]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// SumFloat64 sums a double precision column. PostgreSQL returns double
// precision.
func SumFloat64[E any](c Aggregatable[E, float64]) Agg[E, *float64] {
	return aggregate[E, *float64]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// SumNumeric sums a numeric column. PostgreSQL returns numeric, and both sides
// are the project's own numeric mapping.
func SumNumeric[E, N any](c Aggregatable[E, N]) Agg[E, *N] {
	return aggregate[E, *N]("sum", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// The averages.
//
//	avg(smallint | integer | bigint | numeric) -> numeric
//	avg(real | double precision)               -> double precision
//
// An average of integers is not an integer, and PostgreSQL does not pretend it
// is. The integer forms therefore return the configured numeric type rather
// than float64, for the same reason sum does.

// AvgInt16 averages a smallint column. PostgreSQL returns numeric.
func AvgInt16[E, N any](c Aggregatable[E, int16]) Agg[E, *N] {
	return aggregate[E, *N]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// AvgInt32 averages an integer column. PostgreSQL returns numeric.
func AvgInt32[E, N any](c Aggregatable[E, int32]) Agg[E, *N] {
	return aggregate[E, *N]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// AvgInt64 averages a bigint column. PostgreSQL returns numeric.
func AvgInt64[E, N any](c Aggregatable[E, int64]) Agg[E, *N] {
	return aggregate[E, *N]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// AvgFloat32 averages a real column. PostgreSQL returns double precision.
func AvgFloat32[E any](c Aggregatable[E, float32]) Agg[E, *float64] {
	return aggregate[E, *float64]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// AvgFloat64 averages a double precision column. PostgreSQL returns double
// precision.
func AvgFloat64[E any](c Aggregatable[E, float64]) Agg[E, *float64] {
	return aggregate[E, *float64]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// AvgNumeric averages a numeric column. PostgreSQL returns numeric.
func AvgNumeric[E, N any](c Aggregatable[E, N]) Agg[E, *N] {
	return aggregate[E, *N]("avg", []expr.Node{c.colRef()}, aggOpts{nullable: true})
}

// Walking an expression for aggregates.
//
// An aggregate is legal in a select list and in HAVING, and illegal in WHERE.
// PostgreSQL enforces that, but it does so after a round trip and in terms of
// the statement it received; catching it here says the same thing sooner and in
// terms of the call that made it.

// containsAggregate reports whether a tree holds an aggregate anywhere.
//
// The walk belongs to the expression package, where the node set lives: a node
// type added by a later milestone is then handled here by having been added
// there, rather than by somebody remembering this function exists. It does not
// descend into a nested statement — an aggregate inside a correlated subquery
// is that statement's business, and a WHERE clause containing one is legal.
func containsAggregate(n expr.Node) bool {
	return expr.Holds(n, expr.IsAggregate)
}

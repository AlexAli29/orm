package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Range operators and functions.
//
// The operators are PostgreSQL's own, written by this package and never by a
// caller, with both sides as bind parameters. The names spell out what the
// symbol means, because @> and <@ and &< are not readable in Go and a reader
// should not have to remember which way round they point.
//
//	RangeContains          @>   the range holds the element
//	RangeContainsRange     @>   the range holds the whole of the other
//	RangeContainedBy       <@
//	RangeOverlaps          &&   they have at least one element in common
//	RangeStrictlyLeftOf    <<   every element is below every element of the other
//	RangeStrictlyRightOf   >>
//	RangeNotRightOf        &<   does not extend to the right of the other
//	RangeNotLeftOf         &>   does not extend to the left of the other
//	RangeAdjacent          -|-  they touch without overlapping
//
// Each takes both sides as expressions, so a column can be compared to a value,
// to another column, to a column of a joined or derived source, or to anything a
// composed query produced. The element type is one type parameter shared by both
// sides, which is what stops a Range[int32] being compared to a Range[int64] or
// to a timestamp.
//
// Every one of them is a predicate, so the nullability question they raise is
// SQL's ordinary one: an operator with a NULL operand is NULL, which is not
// true, and WHERE keeps only rows where the answer is true. That is why they
// take [Optional] on both sides — a nullable range and a range read through an
// outer join are both accepted, and neither needs a separate spelling.

// RangeContains builds range @> element.
func RangeContains[A, B, T any](r Optional[A, *Range[T]], v Optional[B, *T]) Predicate[Composed] {
	return rangeOp("@>", r.optItem().Node, v.optItem().Node)
}

// RangeContainsRange builds range @> range: every element of the right is in
// the left. An empty range is contained by every range, including itself.
func RangeContainsRange[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("@>", a.optItem().Node, b.optItem().Node)
}

// RangeContainedBy builds range <@ range.
func RangeContainedBy[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("<@", a.optItem().Node, b.optItem().Node)
}

// RangeOverlaps builds range && range: the two have an element in common.
//
// It is the question a booking system asks. Two half-open ranges [a,b) and
// [c,d) overlap exactly when the periods they describe collide, and an empty
// range overlaps nothing.
func RangeOverlaps[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("&&", a.optItem().Node, b.optItem().Node)
}

// RangeStrictlyLeftOf builds range << range: every element of the left is below
// every element of the right, and they do not touch.
func RangeStrictlyLeftOf[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("<<", a.optItem().Node, b.optItem().Node)
}

// RangeStrictlyRightOf builds range >> range.
func RangeStrictlyRightOf[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp(">>", a.optItem().Node, b.optItem().Node)
}

// RangeNotRightOf builds range &< range: the left does not extend to the right
// of the right, which is to say its upper bound does not exceed the other's.
func RangeNotRightOf[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("&<", a.optItem().Node, b.optItem().Node)
}

// RangeNotLeftOf builds range &> range: the left does not extend to the left of
// the right.
func RangeNotLeftOf[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("&>", a.optItem().Node, b.optItem().Node)
}

// RangeAdjacent builds range -|- range: the two touch without overlapping, so
// their union is one range and their intersection is empty.
func RangeAdjacent[A, B, T any](a Optional[A, *Range[T]], b Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("-|-", a.optItem().Node, b.optItem().Node)
}

func rangeOp(op string, left, right expr.Node) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{Op: op, Left: left, Right: right}}
}

// Range functions.
//
// lower() and upper() are nullable whatever the column says, and that is the
// one thing about them worth stating loudly. A NOT NULL int4range column can
// still hold 'empty', which has no lower bound, and '(,10)', which has no lower
// bound either — so lower() returns NULL for a value the column constraint
// allows. Typing it as non-nullable because the column is NOT NULL would be
// wrong for two of the six shapes a range can take, and would fail at the scan
// rather than at compile time.
//
// The rest — isempty, lower_inc, upper_inc, lower_inf, upper_inf — are total
// over a non-NULL range: every shape has an answer, including 'empty', for
// which isempty is true and the four bound tests are false. So they are
// non-nullable over a non-nullable range, and nullable only when the range
// itself may be NULL.

// RangeLower is PostgreSQL's lower(anyrange).
//
// The result is nullable even when the range is not, because an empty range and
// a range with no lower bound both have no lower bound to return.
func RangeLower[E, T any](r Selectable[E, Range[T]]) Expression[*T, *T] {
	return nullableCall[*T, *T]("lower", r.selectItem().Node)
}

// RangeLowerNull is [RangeLower] over a range that may itself be NULL.
func RangeLowerNull[E, T any](r Selectable[E, *Range[T]]) Expression[*T, *T] {
	return nullableCall[*T, *T]("lower", r.selectItem().Node)
}

// RangeUpper is PostgreSQL's upper(anyrange), nullable for the same reason
// [RangeLower] is.
func RangeUpper[E, T any](r Selectable[E, Range[T]]) Expression[*T, *T] {
	return nullableCall[*T, *T]("upper", r.selectItem().Node)
}

// RangeUpperNull is [RangeUpper] over a range that may itself be NULL.
func RangeUpperNull[E, T any](r Selectable[E, *Range[T]]) Expression[*T, *T] {
	return nullableCall[*T, *T]("upper", r.selectItem().Node)
}

// RangeIsEmpty is PostgreSQL's isempty(anyrange).
func RangeIsEmpty[E, T any](r Selectable[E, Range[T]]) Expression[bool, *bool] {
	return boolCall("isempty", r.selectItem().Node)
}

// RangeIsEmptyNull is [RangeIsEmpty] over a range that may be NULL, which makes
// the answer nullable: a NULL range is neither empty nor non-empty.
func RangeIsEmptyNull[E, T any](r Selectable[E, *Range[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("isempty", r.selectItem().Node)
}

// RangeLowerInc is PostgreSQL's lower_inc(anyrange): whether the lower bound is
// part of the range. It is false for an empty or lower-unbounded range.
func RangeLowerInc[E, T any](r Selectable[E, Range[T]]) Expression[bool, *bool] {
	return boolCall("lower_inc", r.selectItem().Node)
}

// RangeLowerIncNull is [RangeLowerInc] over a range that may be NULL.
func RangeLowerIncNull[E, T any](r Selectable[E, *Range[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("lower_inc", r.selectItem().Node)
}

// RangeUpperInc is PostgreSQL's upper_inc(anyrange).
func RangeUpperInc[E, T any](r Selectable[E, Range[T]]) Expression[bool, *bool] {
	return boolCall("upper_inc", r.selectItem().Node)
}

// RangeUpperIncNull is [RangeUpperInc] over a range that may be NULL.
func RangeUpperIncNull[E, T any](r Selectable[E, *Range[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("upper_inc", r.selectItem().Node)
}

// RangeLowerInf is PostgreSQL's lower_inf(anyrange): whether the range has no
// lower bound.
func RangeLowerInf[E, T any](r Selectable[E, Range[T]]) Expression[bool, *bool] {
	return boolCall("lower_inf", r.selectItem().Node)
}

// RangeLowerInfNull is [RangeLowerInf] over a range that may be NULL.
func RangeLowerInfNull[E, T any](r Selectable[E, *Range[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("lower_inf", r.selectItem().Node)
}

// RangeUpperInf is PostgreSQL's upper_inf(anyrange).
func RangeUpperInf[E, T any](r Selectable[E, Range[T]]) Expression[bool, *bool] {
	return boolCall("upper_inf", r.selectItem().Node)
}

// RangeUpperInfNull is [RangeUpperInf] over a range that may be NULL.
func RangeUpperInfNull[E, T any](r Selectable[E, *Range[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("upper_inf", r.selectItem().Node)
}

func boolCall(fn string, arg expr.Node) Expression[bool, *bool] {
	return Expression[bool, *bool]{node: expr.Call{Func: fn, Args: []expr.Node{arg}}}
}

// nullableCall builds a call whose result is nullable however the argument was
// typed, and marks it so that lifting it through an outer join stays idempotent
// rather than producing a second pointer.
func nullableCall[T, N any](fn string, args ...expr.Node) Expression[T, N] {
	return Expression[T, N]{node: expr.Call{Func: fn, Args: args}, nullSafe: true}
}

// Multirange operators.
//
// PostgreSQL gives a multirange the same containment and overlap operators a
// range has, and gives them across the two: a multirange contains an element, a
// range or another multirange. Only the forms that are unambiguous in Go are
// offered, which is one per question rather than the full cross product.
//
// The set operations — union, intersection, difference — are deliberately
// absent. They return a multirange rather than a boolean, which makes them
// projections rather than predicates, and PostgreSQL's + on multiranges is one
// of the few operators whose result cannot be read without also deciding how
// the ORM should type an anonymous multirange in a select list. Nothing needs
// them yet, and adding an operator that cannot be typed cleanly would cost more
// than it gives.

// MultirangeContains builds multirange @> element.
func MultirangeContains[A, B, T any](m Optional[A, *Multirange[T]], v Optional[B, *T]) Predicate[Composed] {
	return rangeOp("@>", m.optItem().Node, v.optItem().Node)
}

// MultirangeContainsRange builds multirange @> range.
func MultirangeContainsRange[A, B, T any](m Optional[A, *Multirange[T]], r Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("@>", m.optItem().Node, r.optItem().Node)
}

// MultirangeContainsMultirange builds multirange @> multirange.
func MultirangeContainsMultirange[A, B, T any](a Optional[A, *Multirange[T]], b Optional[B, *Multirange[T]]) Predicate[Composed] {
	return rangeOp("@>", a.optItem().Node, b.optItem().Node)
}

// MultirangeContainedBy builds multirange <@ multirange.
func MultirangeContainedBy[A, B, T any](a Optional[A, *Multirange[T]], b Optional[B, *Multirange[T]]) Predicate[Composed] {
	return rangeOp("<@", a.optItem().Node, b.optItem().Node)
}

// MultirangeOverlaps builds multirange && multirange.
func MultirangeOverlaps[A, B, T any](a Optional[A, *Multirange[T]], b Optional[B, *Multirange[T]]) Predicate[Composed] {
	return rangeOp("&&", a.optItem().Node, b.optItem().Node)
}

// MultirangeOverlapsRange builds multirange && range.
func MultirangeOverlapsRange[A, B, T any](m Optional[A, *Multirange[T]], r Optional[B, *Range[T]]) Predicate[Composed] {
	return rangeOp("&&", m.optItem().Node, r.optItem().Node)
}

// MultirangeIsEmpty is PostgreSQL's isempty(anymultirange): whether the
// multirange has no components at all.
func MultirangeIsEmpty[E, T any](m Selectable[E, Multirange[T]]) Expression[bool, *bool] {
	return boolCall("isempty", m.selectItem().Node)
}

// MultirangeIsEmptyNull is [MultirangeIsEmpty] over a multirange that may be
// NULL.
func MultirangeIsEmptyNull[E, T any](m Selectable[E, *Multirange[T]]) Expression[*bool, *bool] {
	return nullableCall[*bool, *bool]("isempty", m.selectItem().Node)
}

// MultirangeMerge is PostgreSQL's range_merge(anymultirange): the smallest
// single range that contains every component.
//
// The result is a range rather than a multirange, which is the point of it: it
// is how a set of disjoint periods becomes the one period spanning them all.
// Over an empty multirange it returns the empty range rather than NULL.
func MultirangeMerge[E, T any](m Selectable[E, Multirange[T]]) Expression[Range[T], *Range[T]] {
	return Expression[Range[T], *Range[T]]{
		node: expr.Call{Func: "range_merge", Args: []expr.Node{m.selectItem().Node}},
	}
}

// MultirangeMergeNull is [MultirangeMerge] over a multirange that may be NULL.
func MultirangeMergeNull[E, T any](m Selectable[E, *Multirange[T]]) Expression[*Range[T], *Range[T]] {
	return nullableCall[*Range[T], *Range[T]]("range_merge", m.selectItem().Node)
}

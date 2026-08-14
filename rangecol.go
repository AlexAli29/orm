package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Range and multirange column descriptors.
//
// They join the same capability lattice every other descriptor belongs to, and
// they are assigned the same way: from the PostgreSQL type rather than from the
// Go type. A range column gets containment and overlap because PostgreSQL
// defines those for ranges; it does not get Like, and it does not get Gt,
// because comparing two ranges by magnitude answers no question anybody asks
// even though PostgreSQL defines an order for indexing.
//
//	RangeCol            equality, membership, the nine range operators, the six
//	                    range functions
//	NullRangeCol        + null tests
//	MultirangeCol       equality, membership, containment and overlap
//	NullMultirangeCol   + null tests
//
// The element type T is a second parameter rather than being folded into the
// value type, so that a descriptor for an int4range column is
// RangeCol[Booking, int32] and its methods take int32 and Range[int32]. That is
// what makes passing a time.Time to an integer range a compile error.
//
// # Why the descriptors carry PostgreSQL type names
//
// A range operator is overloaded on both sides. @> is (anyrange, anyelement)
// and (anyrange, anyrange) and, since PostgreSQL 14, (anymultirange, anything
// again); && is (anyrange, anyrange) and (anyrange, anymultirange). A bind
// parameter arrives with no type, so PostgreSQL picks an overload without
// knowing what is in it — and it picked wrong, resolving quota @> $1 to the
// range-against-range form and then failing to encode an int32 as an
// int4range.
//
// So each descriptor is generated with the catalog names of the types it deals
// in, and every operand it builds is cast to the one that is meant. The names
// come from the catalog during generation, which is the only place that knows
// them; nothing is inferred from the Go type, and nothing relies on which
// overload PostgreSQL would have guessed.

// RangeCol describes a range column of entity E over element type T.
type RangeCol[E, T any] struct {
	Col[E, Range[T]]
	// rangeType and elemType are the column's PostgreSQL type and its subtype,
	// as the catalog names them: "int4range" and "int4". They exist to cast
	// operands so that an overloaded operator resolves to the form meant.
	rangeType, elemType string
}

// NewRangeCol returns a range column descriptor, where rangeType and elemType
// are the catalog names of the column's type and its subtype. Generated code
// calls it.
func NewRangeCol[E, T any](src *Source, name, rangeType, elemType string) RangeCol[E, T] {
	return RangeCol[E, T]{
		Col:       NewCol[E, Range[T]](src, name),
		rangeType: rangeType,
		elemType:  elemType,
	}
}

// elem and rng cast a bind parameter to the type the operator needs.
func (c RangeCol[E, T]) elem(v T) expr.Node {
	return expr.Cast{X: expr.Arg{Value: v}, Type: c.elemType}
}

func (c RangeCol[E, T]) rng(r Range[T]) expr.Node {
	return expr.Cast{X: expr.Arg{Value: r}, Type: c.rangeType}
}

// Contains builds column @> value: the range holds the element.
func (c RangeCol[E, T]) Contains(v T) Predicate[E] { return c.op("@>", c.elem(v)) }

// ContainsRange builds column @> range.
func (c RangeCol[E, T]) ContainsRange(r Range[T]) Predicate[E] {
	return c.op("@>", c.rng(r))
}

// ContainedBy builds column <@ range.
func (c RangeCol[E, T]) ContainedBy(r Range[T]) Predicate[E] { return c.op("<@", c.rng(r)) }

// Overlaps builds column && range: the two have an element in common.
//
// This is the booking question. A requested period collides with a stored one
// exactly when the two ranges overlap, and an empty range overlaps nothing.
func (c RangeCol[E, T]) Overlaps(r Range[T]) Predicate[E] { return c.op("&&", c.rng(r)) }

// StrictlyLeftOf builds column << range.
func (c RangeCol[E, T]) StrictlyLeftOf(r Range[T]) Predicate[E] {
	return c.op("<<", c.rng(r))
}

// StrictlyRightOf builds column >> range.
func (c RangeCol[E, T]) StrictlyRightOf(r Range[T]) Predicate[E] {
	return c.op(">>", c.rng(r))
}

// NotRightOf builds column &< range: the column does not extend to the right of
// the given range.
func (c RangeCol[E, T]) NotRightOf(r Range[T]) Predicate[E] { return c.op("&<", c.rng(r)) }

// NotLeftOf builds column &> range.
func (c RangeCol[E, T]) NotLeftOf(r Range[T]) Predicate[E] { return c.op("&>", c.rng(r)) }

// Adjacent builds column -|- range: the two touch without overlapping.
func (c RangeCol[E, T]) Adjacent(r Range[T]) Predicate[E] { return c.op("-|-", c.rng(r)) }

// OverlapsCol builds column && column, for comparing two range columns of the
// same entity — a self-join looking for periods that collide.
func (c RangeCol[E, T]) OverlapsCol(other ColumnOf[E, Range[T]]) Predicate[E] {
	return c.op("&&", other.colRef())
}

// ContainsCol builds column @> column.
func (c RangeCol[E, T]) ContainsCol(other ColumnOf[E, Range[T]]) Predicate[E] {
	return c.op("@>", other.colRef())
}

func (c RangeCol[E, T]) op(op string, right expr.Node) Predicate[E] {
	return Predicate[E]{node: expr.Infix{Op: op, Left: c.col, Right: right}}
}

// Lower is lower(column), which is nullable however the column is declared: an
// empty range and a range with no lower bound both have none to return.
func (c RangeCol[E, T]) Lower() Value[E, *T] { return c.call("lower") }

// Upper is upper(column), nullable for the same reason [RangeCol.Lower] is.
func (c RangeCol[E, T]) Upper() Value[E, *T] { return c.call("upper") }

func (c RangeCol[E, T]) call(fn string) Value[E, *T] {
	return Value[E, *T]{node: expr.Call{Func: fn, Args: []expr.Node{c.col}}, nullable: true}
}

// IsEmpty is isempty(column). It is total over a non-NULL range, so it is not
// nullable on a NOT NULL column.
func (c RangeCol[E, T]) IsEmpty() Value[E, bool] { return c.boolCall("isempty") }

// LowerInc is lower_inc(column): whether the lower bound is in the range.
func (c RangeCol[E, T]) LowerInc() Value[E, bool] { return c.boolCall("lower_inc") }

// UpperInc is upper_inc(column).
func (c RangeCol[E, T]) UpperInc() Value[E, bool] { return c.boolCall("upper_inc") }

// LowerInf is lower_inf(column): whether the range has no lower bound.
func (c RangeCol[E, T]) LowerInf() Value[E, bool] { return c.boolCall("lower_inf") }

// UpperInf is upper_inf(column).
func (c RangeCol[E, T]) UpperInf() Value[E, bool] { return c.boolCall("upper_inf") }

func (c RangeCol[E, T]) boolCall(fn string) Value[E, bool] {
	return Value[E, bool]{node: expr.Call{Func: fn, Args: []expr.Node{c.col}}}
}

// IsEmptyIs builds isempty(column) = want, so that a range query can ask the
// question in a WHERE clause without a projection.
func (c RangeCol[E, T]) IsEmptyIs(want bool) Predicate[E] {
	return Predicate[E]{node: expr.Binary{
		Op:    expr.OpEq,
		Left:  expr.Call{Func: "isempty", Args: []expr.Node{c.col}},
		Right: expr.Arg{Value: want},
	}}
}

// NullRangeCol describes a nullable range column.
type NullRangeCol[E, T any] struct {
	RangeCol[E, T]
}

// NewNullRangeCol returns a nullable range column descriptor. Generated code
// calls it.
func NewNullRangeCol[E, T any](src *Source, name, rangeType, elemType string) NullRangeCol[E, T] {
	return NullRangeCol[E, T]{RangeCol: NewRangeCol[E, T](src, name, rangeType, elemType)}
}

// IsNull builds column IS NULL.
func (c NullRangeCol[E, T]) IsNull() Predicate[E] { return isNull[E](c.col, true) }

// IsNotNull builds column IS NOT NULL.
func (c NullRangeCol[E, T]) IsNotNull() Predicate[E] { return isNull[E](c.col, false) }

// SetNull assigns SQL NULL to the column, which is a different assignment from
// setting it to the empty range.
func (c NullRangeCol[E, T]) SetNull() Assign[E] { return setNull[E](c.col) }

// IsEmpty is isempty(column) over a nullable range, which is nullable: a NULL
// range is neither empty nor non-empty.
//
// It shadows [RangeCol.IsEmpty] rather than inheriting it, because inheriting
// would type a nullable answer as a plain bool and scan a NULL into it. The
// same applies to the other four bound tests below.
func (c NullRangeCol[E, T]) IsEmpty() Value[E, *bool] { return c.nullBoolCall("isempty") }

// LowerInc is lower_inc(column) over a nullable range.
func (c NullRangeCol[E, T]) LowerInc() Value[E, *bool] { return c.nullBoolCall("lower_inc") }

// UpperInc is upper_inc(column) over a nullable range.
func (c NullRangeCol[E, T]) UpperInc() Value[E, *bool] { return c.nullBoolCall("upper_inc") }

// LowerInf is lower_inf(column) over a nullable range.
func (c NullRangeCol[E, T]) LowerInf() Value[E, *bool] { return c.nullBoolCall("lower_inf") }

// UpperInf is upper_inf(column) over a nullable range.
func (c NullRangeCol[E, T]) UpperInf() Value[E, *bool] { return c.nullBoolCall("upper_inf") }

func (c NullRangeCol[E, T]) nullBoolCall(fn string) Value[E, *bool] {
	return Value[E, *bool]{node: expr.Call{Func: fn, Args: []expr.Node{c.col}}, nullable: true}
}

// MultirangeCol describes a multirange column of entity E over element type T.
type MultirangeCol[E, T any] struct {
	Col[E, Multirange[T]]
	// The three catalog names a multirange operator can need: the column's own
	// type, the range type it is built from, and that range's subtype.
	multiType, rangeType, elemType string
}

// NewMultirangeCol returns a multirange column descriptor, where the three type
// names are the catalog's for the multirange, its range type and that range's
// subtype. Generated code calls it.
func NewMultirangeCol[E, T any](src *Source, name, multiType, rangeType, elemType string) MultirangeCol[E, T] {
	return MultirangeCol[E, T]{
		Col:       NewCol[E, Multirange[T]](src, name),
		multiType: multiType, rangeType: rangeType, elemType: elemType,
	}
}

func (c MultirangeCol[E, T]) cast(v any, ty string) expr.Node {
	return expr.Cast{X: expr.Arg{Value: v}, Type: ty}
}

// multi is cast for a multirange, which is copied on the way in: a slice handed
// to a builder shares its backing array with the caller, and a write to it after
// the predicate was built would change what the predicate means with nothing to
// report. See [Multirange.clone].
func (c MultirangeCol[E, T]) multi(m Multirange[T]) expr.Node {
	return c.cast(m.clone(), c.multiType)
}

// Eq, Ne, In and Set take a copy for the same reason the operators do. They
// shadow the inherited forms rather than adding to the API: the signature is
// the one Col already offers, and only the aliasing differs.

// Eq builds column = multirange.
func (c MultirangeCol[E, T]) Eq(m Multirange[T]) Predicate[E] {
	return Predicate[E]{node: expr.Binary{Op: expr.OpEq, Left: c.col, Right: expr.Arg{Value: m.clone()}}}
}

// Ne builds column <> multirange.
func (c MultirangeCol[E, T]) Ne(m Multirange[T]) Predicate[E] {
	return Predicate[E]{node: expr.Binary{Op: expr.OpNe, Left: c.col, Right: expr.Arg{Value: m.clone()}}}
}

// In builds column IN (multiranges...).
func (c MultirangeCol[E, T]) In(ms ...Multirange[T]) Predicate[E] {
	nodes := make([]expr.Node, 0, len(ms))
	for _, m := range ms {
		nodes = append(nodes, expr.Arg{Value: m.clone()})
	}
	return Predicate[E]{node: expr.In{X: c.col, Values: nodes}}
}

// Set assigns a multirange to the column.
func (c MultirangeCol[E, T]) Set(m Multirange[T]) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: expr.Arg{Value: m.clone()}}}
}

// Contains builds column @> value: some component holds the element.
func (c MultirangeCol[E, T]) Contains(v T) Predicate[E] { return c.op("@>", c.cast(v, c.elemType)) }

// ContainsRange builds column @> range.
func (c MultirangeCol[E, T]) ContainsRange(r Range[T]) Predicate[E] {
	return c.op("@>", c.cast(r, c.rangeType))
}

// ContainsMultirange builds column @> multirange.
func (c MultirangeCol[E, T]) ContainsMultirange(m Multirange[T]) Predicate[E] {
	return c.op("@>", c.multi(m))
}

// ContainedBy builds column <@ multirange.
func (c MultirangeCol[E, T]) ContainedBy(m Multirange[T]) Predicate[E] {
	return c.op("<@", c.multi(m))
}

// Overlaps builds column && multirange.
func (c MultirangeCol[E, T]) Overlaps(m Multirange[T]) Predicate[E] {
	return c.op("&&", c.multi(m))
}

// OverlapsRange builds column && range.
func (c MultirangeCol[E, T]) OverlapsRange(r Range[T]) Predicate[E] {
	return c.op("&&", c.cast(r, c.rangeType))
}

func (c MultirangeCol[E, T]) op(op string, right expr.Node) Predicate[E] {
	return Predicate[E]{node: expr.Infix{Op: op, Left: c.col, Right: right}}
}

// Merge is range_merge(column): the smallest single range covering every
// component. Over a multirange with no components it is the empty range.
func (c MultirangeCol[E, T]) Merge() Value[E, Range[T]] {
	return Value[E, Range[T]]{node: expr.Call{Func: "range_merge", Args: []expr.Node{c.col}}}
}

// IsEmpty is isempty(column): whether the multirange has no components.
func (c MultirangeCol[E, T]) IsEmpty() Value[E, bool] {
	return Value[E, bool]{node: expr.Call{Func: "isempty", Args: []expr.Node{c.col}}}
}

// NullMultirangeCol describes a nullable multirange column.
type NullMultirangeCol[E, T any] struct {
	MultirangeCol[E, T]
}

// NewNullMultirangeCol returns a nullable multirange column descriptor.
// Generated code calls it.
func NewNullMultirangeCol[E, T any](src *Source, name, multiType, rangeType, elemType string) NullMultirangeCol[E, T] {
	return NullMultirangeCol[E, T]{MultirangeCol: NewMultirangeCol[E, T](src, name, multiType, rangeType, elemType)}
}

// IsNull builds column IS NULL.
func (c NullMultirangeCol[E, T]) IsNull() Predicate[E] { return isNull[E](c.col, true) }

// IsNotNull builds column IS NOT NULL.
func (c NullMultirangeCol[E, T]) IsNotNull() Predicate[E] { return isNull[E](c.col, false) }

// SetNull assigns SQL NULL to the column, as distinct from the empty
// multirange.
func (c NullMultirangeCol[E, T]) SetNull() Assign[E] { return setNull[E](c.col) }

// Merge is range_merge(column) over a nullable multirange, which is nullable.
func (c NullMultirangeCol[E, T]) Merge() Value[E, *Range[T]] {
	return Value[E, *Range[T]]{
		node:     expr.Call{Func: "range_merge", Args: []expr.Node{c.col}},
		nullable: true,
	}
}

// IsEmpty is isempty(column) over a nullable multirange, which is nullable.
func (c NullMultirangeCol[E, T]) IsEmpty() Value[E, *bool] {
	return Value[E, *bool]{node: expr.Call{Func: "isempty", Args: []expr.Node{c.col}}, nullable: true}
}

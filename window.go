package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Window functions.
//
// A window function is an aggregate that does not group. It sees a set of rows,
// computes over them, and returns one value for the row it was called on — so
// the statement returns the rows it would have returned anyway, each carrying a
// value computed from its neighbours. That is what makes "the newest post per
// author" one query rather than a correlated subquery per row.
//
// Everything here builds an [Expression] or an [Agg], so windowed values are
// selected, aliased and read back through the same machinery as anything else.
// Where they may appear is narrower: PostgreSQL allows a window function in a
// select list and in ORDER BY, and refuses one in WHERE, GROUP BY, HAVING and a
// join condition — because a window is computed after all of those have already
// decided which rows exist. The builder refuses them there too.
//
// Filtering on a window result therefore means wrapping the statement: compute
// it in a derived table and filter outside. That is the composability M11 is
// for, and it is why there is no QUALIFY here — PostgreSQL has no such clause,
// and inventing one would mean rewriting the caller's query into a shape they
// did not write.

// WindowDef builds a window specification.
//
// The zero specification, from [Window] with nothing added, is OVER () — the
// whole result set as one unordered partition. It is legal and occasionally
// what is wanted, so it is not an error.
type WindowDef struct {
	spec expr.WindowSpec
	errs []error
}

// Window starts a window specification.
//
//	orm.RowNumber().Over(orm.Window().
//	    PartitionBy(orm.Of(Posts.AuthorID)).
//	    OrderBy(orm.Of(Posts.CreatedAt).Desc()))
func Window() *WindowDef { return &WindowDef{} }

// PartitionBy divides the rows into groups the function computes within,
// in the order given.
func (w *WindowDef) PartitionBy(gs ...Grouping[Composed]) *WindowDef {
	for _, g := range gs {
		node := g.groupNode()
		if containsWindow(node) {
			w.errs = append(w.errs, errWindowIn("a PARTITION BY clause"))
			continue
		}
		w.spec.PartitionBy = append(w.spec.PartitionBy, node)
	}
	return w
}

// OrderBy orders the rows inside each partition.
//
// This is the window's own ordering and is not the statement's: it decides
// which rows come before this one for row_number, lag and a frame, and the
// query is free to return its rows in another order entirely.
func (w *WindowDef) OrderBy(os ...Order[Composed]) *WindowDef {
	for _, o := range os {
		if o.IsZero() {
			continue
		}
		w.spec.OrderBy = append(w.spec.OrderBy, o.order)
	}
	return w
}

// Rows sets a frame counted in rows.
func (w *WindowDef) Rows(start, end Bound) *WindowDef { return w.frame(expr.FrameRows, start, end) }

// Range sets a frame counted in values that order equally with the current row.
func (w *WindowDef) Range(start, end Bound) *WindowDef { return w.frame(expr.FrameRange, start, end) }

// Groups sets a frame counted in peer groups.
func (w *WindowDef) Groups(start, end Bound) *WindowDef {
	return w.frame(expr.FrameGroups, start, end)
}

func (w *WindowDef) frame(mode expr.FrameMode, start, end Bound) *WindowDef {
	w.spec.Frame = &expr.Frame{Mode: mode, Start: start.bound, End: end.bound}
	return w
}

// Bound is one edge of a window frame.
type Bound struct {
	bound expr.Bound
}

// UnboundedPreceding starts a frame at the first row of the partition.
func UnboundedPreceding() Bound {
	return Bound{bound: expr.Bound{Kind: expr.UnboundedPreceding}}
}

// UnboundedFollowing ends a frame at the last row of the partition.
func UnboundedFollowing() Bound {
	return Bound{bound: expr.Bound{Kind: expr.UnboundedFollowing}}
}

// CurrentRow bounds a frame at the row being computed.
func CurrentRow() Bound { return Bound{bound: expr.Bound{Kind: expr.CurrentRow}} }

// Preceding bounds a frame n rows, values or groups before the current row.
//
// The offset is an integer rather than an expression because PostgreSQL's
// grammar forbids one referring to a column here, and a checked non-negative
// integer is the whole of what remains. It is rendered as a literal, which is
// the one place this package writes a number into SQL — and it is a number this
// package validated, never a string a caller supplied.
func Preceding(n int) Bound {
	return Bound{bound: expr.Bound{Kind: expr.Preceding, Offset: n}}
}

// Following bounds a frame n rows, values or groups after the current row.
func Following(n int) Bound {
	return Bound{bound: expr.Bound{Kind: expr.Following, Offset: n}}
}

// WindowFn is a window function waiting for the window it computes over.
//
// It is a separate type from [Expression] because a bare row_number() is not a
// value: PostgreSQL requires the OVER clause, and a type that cannot be
// selected until Over has been called is the compiler saying so.
type WindowFn[T, N any] struct {
	agg      expr.Aggregate
	nullSafe bool
}

// Over attaches the window and produces the expression.
//
// A nil specification is OVER (), which is the whole result set — the same
// thing [Window] with nothing added produces.
func (f WindowFn[T, N]) Over(w *WindowDef) Expression[T, N] {
	agg := f.agg
	spec := expr.WindowSpec{}
	var errs []error
	if w != nil {
		spec, errs = w.spec, w.errs
	}
	agg.Over = &spec
	out := Expression[T, N]{node: agg, nullSafe: f.nullSafe}
	if len(errs) > 0 {
		out.node = expr.Fail{Err: errs[0]}
	}
	return out
}

// Over computes an aggregate over a window rather than over a group.
//
//	orm.Sum(Orders.Amount).Over(orm.Window().PartitionBy(orm.Of(Orders.UserID)))
//
// The rows are not collapsed: every row of the statement is returned, carrying
// the aggregate of its window. The result type is unchanged, which is what
// keeps the NULL rules right — a windowed sum can still be NULL and a windowed
// count still cannot.
//
// It also stops being a grouping aggregate, so it no longer belongs in a HAVING
// clause and no longer implies a GROUP BY. Nothing here adds one.
func (a Agg[E, T]) Over(w *WindowDef) Agg[E, T] {
	out := a
	spec := expr.WindowSpec{}
	if w != nil {
		spec = w.spec
	}
	out.agg.Over = &spec
	return out
}

// The ranking functions.
//
// All three number the rows of a partition and all three return bigint, never
// NULL: there is always a row to number, because the function is called on one.

// RowNumber numbers the rows of each partition from one, with no ties.
func RowNumber() WindowFn[int64, *int64] { return windowFn[int64, *int64]("row_number") }

// Rank numbers the rows of each partition, giving tied rows one number and
// leaving a gap after them.
func Rank() WindowFn[int64, *int64] { return windowFn[int64, *int64]("rank") }

// DenseRank numbers the rows of each partition, giving tied rows one number and
// leaving no gap.
func DenseRank() WindowFn[int64, *int64] { return windowFn[int64, *int64]("dense_rank") }

// PercentRank is the rank of the row as a fraction of the partition, from zero
// to one. PostgreSQL returns double precision.
func PercentRank() WindowFn[float64, *float64] {
	return windowFn[float64, *float64]("percent_rank")
}

// CumeDist is the proportion of the partition ordering at or before this row.
func CumeDist() WindowFn[float64, *float64] { return windowFn[float64, *float64]("cume_dist") }

// Ntile divides each partition into n buckets and reports which one this row is
// in. PostgreSQL returns integer.
func Ntile(n int32) WindowFn[int32, *int32] {
	return WindowFn[int32, *int32]{agg: expr.Aggregate{
		Func: "ntile", Args: []expr.Node{expr.Arg{Value: n}},
	}}
}

// Lag reads the value from a row before this one in the window.
//
// # It is nullable
//
// There may be no such row — the first row of every partition has nothing
// before it — and then the result is NULL whatever the expression's own
// nullability is. That is not a conservative choice: it is what PostgreSQL
// returns, and a non-nullable lag would fail on the first row of the first
// partition.
func Lag[E, T, N any](v Typed[E, T, N]) WindowFn[N, N] { return offsetFn[E, T, N](v, "lag", 1) }

// LagN reads the value n rows before this one.
func LagN[E, T, N any](v Typed[E, T, N], n int32) WindowFn[N, N] {
	return offsetFn[E, T, N](v, "lag", n)
}

// Lead reads the value from a row after this one in the window, or NULL when
// there is none.
func Lead[E, T, N any](v Typed[E, T, N]) WindowFn[N, N] { return offsetFn[E, T, N](v, "lead", 1) }

// LeadN reads the value n rows after this one.
func LeadN[E, T, N any](v Typed[E, T, N], n int32) WindowFn[N, N] {
	return offsetFn[E, T, N](v, "lead", n)
}

// FirstValue reads the expression from the first row of the frame.
//
// It is nullable because a frame can be empty — a frame ending before the
// partition starts has no rows in it — and because the value it finds may be
// NULL in its own right.
func FirstValue[E, T, N any](v Typed[E, T, N]) WindowFn[N, N] {
	return valueFn[E, T, N](v, "first_value")
}

// LastValue reads the expression from the last row of the frame.
//
// The default frame ends at the current row, so this is the current row's value
// unless a frame says otherwise — which surprises people often enough to be
// worth saying here.
func LastValue[E, T, N any](v Typed[E, T, N]) WindowFn[N, N] {
	return valueFn[E, T, N](v, "last_value")
}

// NthValue reads the expression from the nth row of the frame, counting from
// one, or NULL when the frame has fewer rows than that.
func NthValue[E, T, N any](v Typed[E, T, N], n int32) WindowFn[N, N] {
	f := valueFn[E, T, N](v, "nth_value")
	f.agg.Args = append(f.agg.Args, expr.Arg{Value: n})
	return f
}

func windowFn[T, N any](name string) WindowFn[T, N] {
	return WindowFn[T, N]{agg: expr.Aggregate{Func: name}, nullSafe: nullSafeAs[T, N]()}
}

// offsetFn builds lag or lead, whose result is the nullable form of the
// expression whatever the expression itself is.
func offsetFn[E, T, N any](v Typed[E, T, N], name string, n int32) WindowFn[N, N] {
	return WindowFn[N, N]{
		agg: expr.Aggregate{Func: name, Args: []expr.Node{
			v.selectItem().Node, expr.Arg{Value: n},
		}},
		nullSafe: true,
	}
}

func valueFn[E, T, N any](v Typed[E, T, N], name string) WindowFn[N, N] {
	return WindowFn[N, N]{
		agg:      expr.Aggregate{Func: name, Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}

// containsWindow reports whether a tree holds a window function, without
// descending into a nested statement — a window inside a subquery is that
// statement's business and is legal where this one's would not be.
func containsWindow(n expr.Node) bool { return expr.Holds(n, expr.IsWindow) }

func errWindowIn(clause string) error {
	return &WindowClauseError{Clause: clause}
}

// WindowClauseError reports a window function where PostgreSQL does not allow
// one.
type WindowClauseError struct {
	// Clause names where the window function was found.
	Clause string
}

// Error describes which window name was used and what went wrong with it.
func (e *WindowClauseError) Error() string {
	return "a window function cannot appear in " + e.Clause +
		"; windows are computed after WHERE, GROUP BY and HAVING have decided which rows exist," +
		" so filtering on one means computing it in a subquery and filtering outside — which is what a derived table is for"
}

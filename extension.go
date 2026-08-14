package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// The extension boundary.
//
// PostgreSQL's own types were added to this package one at a time — inet, jsonb,
// tsvector, the ranges — because each is part of the server everybody has. An
// extension is different: PostGIS is a hundred functions and a type model of its
// own, and putting that here would make every project read past it to find the
// query API.
//
// So an extension lives in its own package, and this is the only door it needs.
// It is deliberately narrow: an extension can turn typed expressions and values
// into arguments, build a call or an operator over them, and get back a typed
// expression or predicate carrying the same entity tag its arguments did. It
// cannot reach the tree, the scope, the writer or the argument list, which is
// what keeps "one compiler" true — an extension composes the same nodes the rest
// of this package does, is rendered by the same writer, and is checked by the
// same scope and nullability rules.
//
// What crosses the boundary is a name and a shape. The name is written into the
// statement as syntax, so it comes from the extension's own constructors and
// never from a caller; every value still becomes a bind parameter. That is the
// same contract the functions in this package hold themselves to.
//
// The entity tag travels through as a type parameter. An extension writes
//
//	func DWithin[E any](c GeomCol[E], p Point, m float64) orm.Predicate[E]
//
// and the result goes into that entity's query and no other — the extension
// gets the tag checking for free rather than reimplementing it.

// Arg is one operand handed to an extension's expression builder.
//
// It is opaque on purpose. An extension needs to pass expressions along and to
// bind values; it does not need to see the tree, and a type it could inspect
// would be a tree this package could no longer change.
type Arg struct {
	item expr.SelectItem
}

// ArgOf makes an argument of a typed expression, keeping the nullability it
// already carries.
//
// A column read through an outer join arrives here already knowing it can be
// absent, so an extension function over it produces a nullable result without
// the extension having to reason about joins.
func ArgOf[E, T any](v Selectable[E, T]) Arg {
	return Arg{item: v.selectItem()}
}

// ArgOpt makes an argument of an expression's nullable form, which is what an
// outer join needs.
func ArgOpt[E, N any](v Optional[E, N]) Arg {
	return Arg{item: v.optItem()}
}

// ArgValue makes an argument of a Go value, which becomes a bind parameter.
//
// There is no path by which a value reaches the statement as text. An extension
// that wants a literal in the syntax — a dimension keyword, a unit in a grammar
// position — has to write it through its own closed set, not through this.
func ArgValue[V any](v V) Arg {
	return Arg{item: expr.SelectItem{Node: expr.Arg{Value: v}}}
}

// ArgCast makes an argument of a Go value with an explicit type.
//
// It exists because PostgreSQL resolves an overloaded function by the types of
// its arguments, and a bare parameter has none. ST_Distance has a geometry form
// and a geography form and a deprecated text form; handed two untyped
// parameters the server picks the last one and fails on bytes that were never
// text. Saying the type is how an extension makes the server choose the
// function it meant.
//
// The type name is syntax and comes from the extension, never from a caller.
// It is written as a quoted identifier — schema-qualified if it contains a dot,
// with an array suffix outside the quotes — so a name is a name and cannot
// become anything else.
func ArgCast[V any](v V, pgType string) Arg {
	return Arg{item: expr.SelectItem{Node: expr.Cast{X: expr.Arg{Value: v}, Type: pgType}}}
}

// ArgAs casts an expression argument to a named type.
func ArgAs(a Arg, pgType string) Arg {
	return Arg{item: expr.SelectItem{
		Node:     expr.Cast{X: a.item.Node, Type: pgType},
		Nullable: a.item.Nullable,
	}}
}

// ArgRaw makes an argument of SQL the extension itself wrote.
//
// It exists for the grammar positions PostgreSQL does not accept a parameter
// in. The fragment comes from the extension's own code — never from a caller —
// and its own placeholders are renumbered into the surrounding statement like
// any other raw fragment, so values inside it are still parameters.
func ArgRaw(sql string, args ...any) Arg {
	return Arg{item: expr.SelectItem{Node: expr.Raw{SQL: sql, Args: args}}}
}

// ArgFail makes an argument that is a mistake.
//
// An extension's constructors return expressions rather than errors, because a
// constructor that returned an error would not compose into the expression it
// is part of. So the mistake travels in the tree instead, and the compiler —
// which has an error return — reports it when the statement is built. An
// expression containing one never compiles to SQL.
//
// This is how an extension refuses something it can see is wrong: comparing
// geometries in two coordinate systems, measuring a distance in units the
// column does not have.
func ArgFail(err error) Arg {
	return Arg{item: expr.SelectItem{Node: expr.Fail{Err: err}}}
}

// ArgNullable reports whether an argument can read back as NULL, so that an
// extension can decide the nullability of a result it builds from several.
func ArgNullable(a Arg) bool { return a.item.Nullable }

// AnyNullable reports whether any of the arguments can read back as NULL.
//
// Most SQL functions are strict — give one a NULL and the answer is NULL — so
// this is how an extension carries source-induced nullability through a
// function the compiler has never heard of.
func AnyNullable(args ...Arg) bool {
	for _, a := range args {
		if a.item.Nullable {
			return true
		}
	}
	return false
}

// Entity-tagged results.
//
// These are what a column descriptor's methods return, and they carry the
// entity tag so that a predicate built from one table's column cannot be handed
// to another table's query.

// Fn builds a typed function call over entity E whose result cannot be NULL.
//
//	postgis.Area(c)  ->  orm.Fn[E, float64]("ST_Area", orm.ArgOf(c))
func Fn[E, T any](name string, args ...Arg) Value[E, T] {
	return Value[E, T]{node: expr.Call{Func: name, Args: argNodes(args)}}
}

// FnNull builds a typed function call over entity E whose result can be NULL.
//
// The result type is a pointer, which is the only Go shape that tells SQL NULL
// from the type's zero value — and for a spatial measurement the difference
// between "no answer" and 0 is the difference between "there is no geometry
// here" and "it is exactly here".
func FnNull[E, T any](name string, args ...Arg) Value[E, *T] {
	return Value[E, *T]{node: expr.Call{Func: name, Args: argNodes(args)}, nullable: true}
}

// Op builds a typed infix operator over entity E whose result cannot be NULL.
//
// The spelling is syntax and comes from the extension, never from a caller.
func Op[E, T any](op string, left, right Arg) Value[E, T] {
	return Value[E, T]{node: expr.Infix{Op: op, Left: left.item.Node, Right: right.item.Node}}
}

// OpNull builds a typed infix operator over entity E whose result can be NULL.
func OpNull[E, T any](op string, left, right Arg) Value[E, *T] {
	return Value[E, *T]{
		node:     expr.Infix{Op: op, Left: left.item.Node, Right: right.item.Node},
		nullable: true,
	}
}

// ValueOf reads an argument the extension already built as a typed value over
// entity E.
//
// An extension composes several calls into one expression and then needs a
// handle on the result: this is that handle. It states the Go type, which is
// the extension's job — it is the side that knows what its own functions
// return, and every one of those claims is checked against the server in its
// own tests.
func ValueOf[E, T any](a Arg) Value[E, T] {
	return Value[E, T]{node: a.item.Node, nullable: a.item.Nullable}
}

// ValueOfNull is [ValueOf] for a result that can be NULL, which the destination
// then has to be able to hold.
func ValueOfNull[E, T any](a Arg) Value[E, *T] {
	return Value[E, *T]{node: a.item.Node, nullable: true}
}

// BoolOf reads a predicate as a value.
//
// A predicate is a boolean expression, and sometimes the boolean is the answer
// rather than the filter: a projection that reports whether each row matched, a
// GROUP BY over a condition, an assignment of a comparison to a boolean column.
// The result is nullable-free at the type level and three-valued in SQL — a
// predicate over a NULL is UNKNOWN, which reads back as NULL — so it is [Value]
// over bool only where the operands cannot be NULL, and callers with nullable
// operands should read it through the nullable form.
func BoolOf[E any](p Predicate[E]) Value[E, bool] {
	if p.err != nil {
		return Value[E, bool]{node: expr.Fail{Err: p.err}}
	}
	return Value[E, bool]{node: p.node}
}

// BoolOfNull is [BoolOf] where the predicate's operands can be NULL, so the
// answer can be UNKNOWN and reads back as a nil pointer.
func BoolOfNull[E any](p Predicate[E]) Value[E, *bool] {
	if p.err != nil {
		return Value[E, *bool]{node: expr.Fail{Err: p.err}, nullable: true}
	}
	return Value[E, *bool]{node: p.node, nullable: true}
}

// AggNull builds an aggregate over entity E whose result can be NULL.
//
// Every aggregate but count is NULL over an empty group, which is why there is
// only the nullable form: an extension aggregate that claimed otherwise would
// be claiming its group is never empty. The result is an ordinary [Agg], so it
// carries DISTINCT, FILTER and the HAVING comparisons the rest of this package
// gives aggregates.
func AggNull[E, T any](name string, args ...Arg) Agg[E, *T] {
	return aggregate[E, *T](name, argNodes(args), aggOpts{nullable: true})
}

// FnPredicate builds a predicate over entity E from a function call.
//
// The result is an ordinary [Predicate], so it composes with And, Or and Not
// and goes wherever a predicate over E goes. SQL's three-valued logic is
// unchanged: a spatial predicate over a NULL geometry is UNKNOWN, and this does
// not pretend it is false.
func FnPredicate[E any](name string, args ...Arg) Predicate[E] {
	return Predicate[E]{node: expr.Call{Func: name, Args: argNodes(args)}}
}

// OpPredicate builds a predicate over entity E from an operator.
func OpPredicate[E any](op string, left, right Arg) Predicate[E] {
	return Predicate[E]{node: expr.Infix{Op: op, Left: left.item.Node, Right: right.item.Node}}
}

// Free-standing results.
//
// These carry no entity tag and are what an extension's cross-source functions
// return: the operands may come from different tables, so there is no single
// entity to name, and the composed statement's scope check is what validates
// them instead. It is the same trade [Matches] and [Gt] make in this package.

// FnExpr builds a typed function call whose result cannot be NULL.
//
// The nullable form is *T, which is what the result becomes when read through
// an outer join.
func FnExpr[T any](name string, args ...Arg) Expression[T, *T] {
	return Expression[T, *T]{node: expr.Call{Func: name, Args: argNodes(args)}}
}

// FnExprNull builds a typed function call whose result can be NULL.
//
// Its nullable form is itself, because a value that is already a pointer
// absorbs an outer join's NULL without widening again.
func FnExprNull[T any](name string, args ...Arg) Expression[*T, *T] {
	return Expression[*T, *T]{node: expr.Call{Func: name, Args: argNodes(args)}, nullSafe: true}
}

// OpExpr builds a typed infix operator whose result cannot be NULL.
func OpExpr[T any](op string, left, right Arg) Expression[T, *T] {
	return Expression[T, *T]{node: expr.Infix{Op: op, Left: left.item.Node, Right: right.item.Node}}
}

// OpExprNull builds a typed infix operator whose result can be NULL.
func OpExprNull[T any](op string, left, right Arg) Expression[*T, *T] {
	return Expression[*T, *T]{
		node:     expr.Infix{Op: op, Left: left.item.Node, Right: right.item.Node},
		nullSafe: true,
	}
}

// A predicate over the sources a composed query names is FnPredicate or
// OpPredicate instantiated at [Composed]. There is no separate spelling for it,
// because a second name for the same function is a second thing to keep in step.

func argNodes(args []Arg) []expr.Node {
	out := make([]expr.Node, 0, len(args))
	for _, a := range args {
		out = append(out, a.item.Node)
	}
	return out
}

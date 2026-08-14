package orm

import (
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// Write expressions.
//
// M4 assigned values: Users.Name.Set("Alex") binds a parameter and writes it.
// This adds the other half — assigning an expression, so that a counter can be
// incremented without reading it first, and an upsert can compute a new value
// from the old one and the rejected one.
//
// It is the same expression system the read side uses. An assignment's
// right-hand side is a [Value], which is what a column, an arithmetic
// expression, an EXCLUDED reference and a raw fragment all already produce; the
// only thing added here is the typed way to build the arithmetic and the rule
// about where each source is visible.

// SetExpr assigns the result of an expression to the column.
//
//	Counters.Value.SetExpr(Counters.Value.Add(1))
//
// The expression's type is the column's, so assigning text to an integer
// column does not compile. Its entity is the column's too, so an expression
// over another table cannot reach this assignment.
//
// A literal inside the expression is still a parameter: Add(1) compiles to
// "value" + $1 rather than to text.
func (c Col[E, V]) SetExpr(v Value[E, V]) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: v.node}}
}

// SetExpr assigns the result of an expression to a nullable column.
//
// The expression may be nullable, because the column is: its value type is a
// pointer for exactly that reason.
func (c NullCol[E, V]) SetExpr(v Value[E, *V]) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: v.node}}
}

// SetExpr assigns the result of an expression to a nullable column.
func (c NullOrdCol[E, V]) SetExpr(v Value[E, *V]) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: v.node}}
}

// SetExpr assigns the result of an expression to a nullable column.
func (c NullTextCol[E]) SetExpr(v Value[E, *string]) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: v.node}}
}

// Arithmetic.
//
// These are on the ordered columns rather than on every column, because the
// capability model says which PostgreSQL types support them: adding to a jsonb
// column is not a thing to offer and then have the server refuse.
//
// The result type is the operand type, which is true of PostgreSQL's arithmetic
// between two values of one type for every type this project maps — int2 + int2
// is int2, numeric + numeric is numeric. Where PostgreSQL would promote, the
// operands are of different types and the expression cannot be built here at
// all, so there is nothing to mistype.

// Add builds column + value.
func (c OrdCol[E, V]) Add(v V) Value[E, V] { return c.arith(expr.OpAdd, expr.Arg{Value: v}) }

// Sub builds column - value.
func (c OrdCol[E, V]) Sub(v V) Value[E, V] { return c.arith(expr.OpSub, expr.Arg{Value: v}) }

// Mul builds column * value.
func (c OrdCol[E, V]) Mul(v V) Value[E, V] { return c.arith(expr.OpMul, expr.Arg{Value: v}) }

// Div builds column / value.
//
// Integer division truncates in PostgreSQL, and division by zero is an error
// the server raises rather than a value it invents. Both are PostgreSQL's
// semantics and neither is smoothed over here.
func (c OrdCol[E, V]) Div(v V) Value[E, V] { return c.arith(expr.OpDiv, expr.Arg{Value: v}) }

// AddCol builds column + other, over two expressions of the same entity and
// type.
//
// The operand is a [Selectable] rather than a [ColumnOf] because nullability
// has to travel with it: a nullable column satisfies Selectable[E, *V] and not
// Selectable[E, V], so adding one to a non-nullable column does not compile.
// SQL arithmetic propagates NULL, and a sum with a nullable operand is nullable
// whatever the other side is.
func (c OrdCol[E, V]) AddCol(other Selectable[E, V]) Value[E, V] {
	return c.arith(expr.OpAdd, other.selectItem().Node)
}

// SubCol builds column - other.
func (c OrdCol[E, V]) SubCol(other Selectable[E, V]) Value[E, V] {
	return c.arith(expr.OpSub, other.selectItem().Node)
}

func (c OrdCol[E, V]) arith(op expr.ArithOp, right expr.Node) Value[E, V] {
	return Value[E, V]{node: expr.Arith{Op: op, Left: c.col, Right: right}}
}

// Excluded refers to the value the conflicting row would have had.
//
//	Users.Name.SetExpr(orm.Excluded(Users.Name))
//
// PostgreSQL exposes it as the EXCLUDED pseudo-table inside ON CONFLICT DO
// UPDATE, and nowhere else. The type is the column's own, so an EXCLUDED
// reference to one column cannot be assigned to a column of another type — and
// because it is a distinct node rather than a raw string, a statement that used
// it outside a conflict clause is refused by the builder rather than by the
// server.
//
// The argument is a [Selectable] so that nullability travels with it. A
// nullable column satisfies Selectable[E, *V], so Excluded of one infers
// Value[E, *V] and the reference cannot silently claim it is never NULL.
func Excluded[E, V any](c interface {
	Selectable[E, V]
	colRef() expr.Column
},
) Value[E, V] {
	return Value[E, V]{node: expr.Excluded{Name: c.colRef().Name}}
}

// containsExcluded reports whether a tree refers to the EXCLUDED pseudo-table,
// which is legal only inside ON CONFLICT DO UPDATE.
func containsExcluded(n expr.Node) bool {
	switch n := n.(type) {
	case nil:
		return false
	case expr.Excluded:
		return true
	case expr.Binary:
		return containsExcluded(n.Left) || containsExcluded(n.Right)
	case expr.Arith:
		return containsExcluded(n.Left) || containsExcluded(n.Right)
	case expr.Unary:
		return containsExcluded(n.X)
	case expr.Group:
		for _, item := range n.Items {
			if containsExcluded(item) {
				return true
			}
		}
		return false
	case expr.In:
		if containsExcluded(n.X) {
			return true
		}
		for _, v := range n.Values {
			if containsExcluded(v) {
				return true
			}
		}
		return false
	case expr.Between:
		return containsExcluded(n.X) || containsExcluded(n.Lo) || containsExcluded(n.Hi)
	case expr.Aggregate:
		for _, a := range n.Args {
			if containsExcluded(a) {
				return true
			}
		}
		return containsExcluded(n.Filter)
	default:
		return false
	}
}

// checkAssignment reports the ways an assignment could be wrong for the clause
// it is in.
//
// The column has to be one this table may be told, which is what the metadata
// already knows; and the expression has to name only sources the clause can
// see. EXCLUDED is the one that differs by clause, which is why it is passed in
// rather than assumed.
func (r *Repo[E]) checkAssignment(a Assign[E], excludedAllowed bool) error {
	if err := a.Err(); err != nil {
		return err
	}
	if a.IsZero() {
		return fmt.Errorf("assignment names no column")
	}
	if err := r.checkWritable(a.Column()); err != nil {
		return err
	}
	if !excludedAllowed && containsExcluded(a.assignment.Value) {
		return fmt.Errorf("%s.%s is assigned from EXCLUDED, which only exists inside ON CONFLICT DO UPDATE",
			r.meta.Table, a.Column())
	}
	if containsAggregate(a.assignment.Value) {
		return fmt.Errorf("%s.%s is assigned an aggregate, which has no group to be taken over here",
			r.meta.Table, a.Column())
	}
	return nil
}

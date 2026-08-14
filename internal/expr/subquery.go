package expr

import "fmt"

// Nesting.
//
// M11 is the milestone where a statement stops being a leaf. A query becomes a
// FROM item, a value, a membership test or a WITH item, and the thing that
// makes all of those one feature rather than five is that they nest the same
// way: the inner statement is written into the *enclosing* writer.
//
// That is the whole placeholder story. Nothing here renders an inner statement
// to a finished string and splices it in — there is no inner "$1" to renumber,
// because the inner statement never had a parameter list of its own. One writer
// allocates every placeholder in the order the SQL is written, so the numbering
// is whatever the traversal produced and cannot disagree with the text.

// Subquery is a statement that can be nested inside another.
//
// It is closed by unexported methods, which is what keeps the set of things
// that can be nested to the ones this package can compile and analyse: a
// SELECT, a set operation over SELECTs, and — for a data-modifying WITH item —
// an INSERT, UPDATE or DELETE with a RETURNING clause.
type Subquery interface {
	// write renders the statement into an existing writer, sharing its scope
	// and its parameter numbering.
	write(w *writer) error
	// bound reports the sources the statement itself introduces.
	bound(add func(*Source))
	// free reports the sources the statement refers to but does not introduce.
	// Those are its correlation: what it needs from an enclosing query.
	free(add func(*Source))
	// each reports every source appearing anywhere in the statement, bound or
	// not, so that a CTE's dependencies can be found even when it selects from
	// them rather than correlating to them.
	each(add func(*Source))
	// resultArity reports how many columns the statement returns, or -1 when
	// the statement cannot say. It is what a scalar or IN subquery checks
	// before PostgreSQL has to.
	resultArity() int
}

// SubqueryValue is a statement used where a value is expected.
//
// PostgreSQL calls it a scalar subquery, and its semantics are the reason this
// node exists rather than the caller pasting a SELECT into a Raw fragment:
// no row yields NULL, one row yields the value, and two rows are an error the
// server raises at run time. The first of those is why the typed wrapper above
// this package is always nullable — a subquery over a NOT NULL column is still
// NULL when it matches nothing.
type SubqueryValue struct {
	Sub Subquery
}

func (SubqueryValue) node() {}

// InSubquery is a membership test against the rows a statement returns.
//
// It is not [Exists] with the comparison moved inside, and it is not rewritten
// into one. NOT IN over a subquery that yields a NULL is UNKNOWN for every row,
// where NOT EXISTS is TRUE; the two statements answer different questions and
// PostgreSQL is right about both.
type InSubquery struct {
	X   Node
	Sub Subquery
	Not bool
}

func (InSubquery) node() {}

// subqueryValue writes a scalar subquery.
func (w *writer) subqueryValue(n SubqueryValue) {
	if n.Sub == nil {
		w.fail(fmt.Errorf("scalar subquery has no statement"))
		return
	}
	if arity := n.Sub.resultArity(); arity > 1 {
		w.fail(fmt.Errorf("a scalar subquery returns one column and this one returns %d", arity))
		return
	}
	w.b.WriteByte('(')
	if err := n.Sub.write(w); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteByte(')')
}

// inSubquery writes a membership test against a statement.
func (w *writer) inSubquery(n InSubquery) {
	if n.Sub == nil {
		w.fail(fmt.Errorf("IN subquery has no statement"))
		return
	}
	if arity := n.Sub.resultArity(); arity > 1 {
		w.fail(fmt.Errorf("an IN subquery compares one column and this one returns %d", arity))
		return
	}
	w.node(n.X, true)
	if n.Not {
		w.b.WriteString(" NOT")
	}
	w.b.WriteString(" IN (")
	if err := n.Sub.write(w); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteByte(')')
}

// derived writes a subquery in a FROM clause.
//
// LATERAL is written by the caller, because whether a FROM item may refer to
// the items before it is a property of how it was attached rather than of the
// statement inside it.
func (w *writer) derived(s *Source) {
	if s.sub == nil {
		w.fail(fmt.Errorf("derived table %q has no statement", s.alias))
		return
	}
	w.b.WriteByte('(')
	if err := s.sub.write(w); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteString(") AS ")
	w.ident(s.Ref())
}

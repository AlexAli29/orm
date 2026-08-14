package expr

import "fmt"

// Value expressions.
//
// M2 needed one kind of expression: a predicate, which is a node that compiles
// to a boolean. A projection needs another: a node that compiles to a value and
// is selected. They are the same trees — a column is a column whether it is
// being compared or returned — so this adds nodes rather than a second
// expression language.
//
// What is new is that a selected node has to carry more than its shape. The
// caller has to know what Go type reads it back and whether it can be NULL, and
// those are decided by the public wrappers rather than here: this package holds
// the tree, and the tree has never known what a value means in Go.

// SelectItem is one entry of a select list.
//
// The alias is a result name, which is a different thing from a table alias:
// it names the column of the result, is never referred to by another clause in
// these statements, and belongs to the item rather than to the source. Keeping
// them separate is what stops Users.As("u") and Users.Email.As("email") from
// being the same operation with two meanings.
type SelectItem struct {
	Node  Node
	Alias string
	// Nullable records that the Go type reading this item back can hold a
	// NULL. The tree has never known what a Go type is and still does not:
	// this is one bit the typed layer above computes and hands down, because
	// it is the only thing that can, and because the compiler needs it to
	// refuse a select list that reads an outer-joined column into a
	// destination that cannot hold the NULL the join can produce.
	Nullable bool
}

// Aggregate is a function applied over the rows of a group.
//
// It is a node like any other, so it can be selected, compared in a HAVING
// clause, or nested inside arithmetic. What it cannot be is used in a WHERE
// clause; PostgreSQL rejects that, and so does the builder above this package,
// because the message arrives sooner and in the caller's vocabulary.
type Aggregate struct {
	// Func is the PostgreSQL function name, written by this package rather
	// than by a caller: it becomes syntax, so it never comes from outside.
	Func string
	// Args are the arguments. Empty with Star renders count(*).
	Args []Node
	// Star renders * as the single argument, which only count accepts.
	Star bool
	// Distinct renders DISTINCT before the arguments.
	Distinct bool
	// Filter is the FILTER (WHERE ...) clause, which restricts the rows this
	// aggregate sees without restricting the statement's own rows.
	Filter Node
	// Over, when set, makes this a window function: it computes over the rows
	// the specification describes and returns a value for each row rather than
	// collapsing them into one. A node with it set is not a grouping aggregate
	// and is not treated as one anywhere.
	Over *WindowSpec
}

func (Aggregate) node() {}

// ArithOp is a binary arithmetic operator.
type ArithOp uint8

// The arithmetic PostgreSQL performs on the types this project maps.
const (
	arithInvalid ArithOp = iota
	OpAdd
	OpSub
	OpMul
	OpDiv
)

var arithSQL = map[ArithOp]string{
	OpAdd: "+",
	OpSub: "-",
	OpMul: "*",
	OpDiv: "/",
}

// String returns the operator's PostgreSQL spelling.
func (o ArithOp) String() string {
	if s, ok := arithSQL[o]; ok {
		return s
	}
	return "?"
}

// Arith is an infix arithmetic expression.
//
// It is separate from [Binary] because that node compiles a comparison and this
// one compiles a value; sharing them would make an operator set that answers
// two different questions and a compiler that has to ask which.
type Arith struct {
	Op          ArithOp
	Left, Right Node
}

func (Arith) node() {}

// writeAggregate renders an aggregate call.
func (w *writer) aggregate(n Aggregate) {
	if n.Func == "" {
		w.fail(fmt.Errorf("aggregate has no function name"))
		return
	}
	// The name is written literally because it comes from this package's own
	// constructors and never from a caller. Quoting it would produce
	// "count"(*), which PostgreSQL reads as a user-defined function.
	w.b.WriteString(n.Func)
	w.b.WriteByte('(')
	switch {
	case n.Star && len(n.Args) > 0:
		w.fail(fmt.Errorf("aggregate %s has both * and arguments", n.Func))
		return
	case n.Star:
		w.b.WriteByte('*')
	case len(n.Args) == 0:
		// A window function may genuinely take none: row_number() reads no
		// value, it counts rows. An aggregate with no arguments and no star is
		// still a mistake, because there would be nothing for it to aggregate.
		if n.Over == nil {
			w.fail(fmt.Errorf("aggregate %s has no arguments", n.Func))
			return
		}
	default:
		if n.Distinct {
			w.b.WriteString("DISTINCT ")
		}
		for i, a := range n.Args {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.node(a, false)
		}
	}
	w.b.WriteByte(')')

	if n.Filter != nil && !IsTrue(n.Filter) {
		w.b.WriteString(" FILTER (WHERE ")
		w.node(n.Filter, false)
		w.b.WriteByte(')')
	}
	// OVER comes after FILTER, which is the order PostgreSQL's grammar fixes.
	// An empty specification is still a window — OVER () is the whole result
	// set — so the layer above always allocates one, and nil means there is no
	// window at all.
	if n.Over != nil {
		w.writeWindow(n.Over)
	}
}

// arith renders an arithmetic expression.
//
// Both operands are parenthesised when they are themselves compound, so that
// the tree the caller built is the tree PostgreSQL evaluates rather than
// whatever its precedence rules would have made of the flattened text.
func (w *writer) arith(n Arith, nested bool) {
	if _, ok := arithSQL[n.Op]; !ok {
		w.fail(fmt.Errorf("unknown arithmetic operator"))
		return
	}
	if nested {
		w.b.WriteByte('(')
	}
	w.node(n.Left, true)
	w.b.WriteByte(' ')
	w.b.WriteString(n.Op.String())
	w.b.WriteByte(' ')
	w.node(n.Right, true)
	if nested {
		w.b.WriteByte(')')
	}
}

// selectList renders a select list, aliasing the items that asked for it.
func (w *writer) selectList(items []SelectItem) {
	for i, it := range items {
		if i > 0 {
			w.b.WriteString(", ")
		}
		if it.Node == nil {
			w.fail(fmt.Errorf("select item %d has no expression", i))
			return
		}
		w.node(it.Node, false)
		if it.Alias != "" {
			w.b.WriteString(" AS ")
			w.ident(it.Alias)
		}
	}
}

package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// writer renders a tree into SQL text and a parameter list.
//
// It never writes a caller's value into the text. Every value becomes a
// placeholder and joins Args, so there is no path by which a string from the
// application reaches the statement as syntax.
type writer struct {
	b     strings.Builder
	args  []any
	err   error
	scope Scope
	// withNames is the CTE names in force, innermost last. A reference to a
	// named query is written as a bare identifier, so a reference nothing
	// declares renders a statement naming a relation that does not exist —
	// valid-looking text that fails at the server. The names are a stack
	// because a WITH item declared by an enclosing statement is in force inside
	// the statements nested in it.
	withNames []map[string]bool
}

// noNames is what a statement with no WITH clause takes out of force: nothing.
// It is a package-level value so that the common path allocates no closure.
var noNames = func() {}

// pushWith brings a set of declared CTE names into force, and returns the
// function that takes them out again.
func (w *writer) pushWith(names map[string]bool) func() {
	w.withNames = append(w.withNames, names)
	return func() { w.withNames = w.withNames[:len(w.withNames)-1] }
}

// declaresCTE reports whether a named query is in force here.
func (w *writer) declaresCTE(name string) bool {
	for _, frame := range w.withNames {
		if frame[name] {
			return true
		}
	}
	return false
}

func (w *writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// arg appends a parameter and writes its placeholder.
func (w *writer) arg(v any) {
	w.args = append(w.args, v)
	w.b.WriteByte('$')
	w.b.WriteString(strconv.Itoa(len(w.args)))
}

// ident writes a quoted PostgreSQL identifier.
//
// Identifiers reach this package from the catalog by way of the generator, so
// they are already the names PostgreSQL itself reported. Quoting them anyway
// costs nothing and means the one place identifiers become syntax has a single,
// checkable rule.
func (w *writer) ident(name string) {
	quoted, err := QuoteIdentifier(name)
	if err != nil {
		w.fail(err)
		return
	}
	w.b.WriteString(quoted)
}

// QuoteIdentifier renders a name as a quoted PostgreSQL identifier.
//
// It is the one function that turns a string into syntax rather than into a
// parameter, which is why it is separate and why it refuses rather than
// improvising: an empty name would produce "" and a NUL byte would truncate the
// statement at the protocol level. Every quote inside is doubled, so nothing in
// the name can end the identifier early.
func QuoteIdentifier(name string) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("empty SQL identifier")
	case strings.ContainsRune(name, 0):
		return "", fmt.Errorf("SQL identifier %q contains a NUL byte", name)
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// source writes a FROM item, binding its alias when it has one.
//
// The three kinds differ here and nowhere else. Everything downstream — how a
// column of the item is qualified, how the item is checked against the scope,
// how a join attaches it — is written once and does not ask what kind it is.
func (w *writer) source(s *Source) {
	if s == nil {
		w.fail(fmt.Errorf("expression refers to no source"))
		return
	}
	if s.aliasErr != nil {
		w.fail(s.aliasErr)
		return
	}
	switch s.kind {
	case SourceDerived:
		w.derived(s)
		return
	case SourceCTE:
		if !w.declaresCTE(s.cTEName) {
			// The reference would be written as a bare identifier and PostgreSQL
			// would look for a relation of that name. Saying so here names the
			// query that is missing; saying nothing produces a statement that
			// reads correctly and fails.
			w.fail(fmt.Errorf("the statement selects from the named query %q without declaring it; pass it to With, in the statement that uses it", s.cTEName))
			return
		}
		w.ident(s.cTEName)
		if s.alias != "" && s.alias != s.cTEName {
			w.b.WriteString(" AS ")
			w.ident(s.alias)
		}
		return
	}
	if s.schema != "" {
		w.ident(s.schema)
		w.b.WriteByte('.')
	}
	w.ident(s.table)
	if s.alias != "" {
		w.b.WriteString(" AS ")
		w.ident(s.alias)
	}
}

// column writes a column reference, qualified by the occurrence it belongs to
// and only if that occurrence is in scope.
func (w *writer) column(c Column) {
	if c.Source == nil {
		w.fail(fmt.Errorf("column %q refers to no source", c.Name))
		return
	}
	if !w.scope.Visible(c.Source) {
		w.fail(&ScopeError{Column: c.Name, Source: c.Source, Visible: w.scope.Sources()})
		return
	}
	// A derived table and a CTE reference know which columns they have,
	// because the statement that defined them said so. A table does not, and
	// does not need to: the generator proved its descriptors against the
	// catalog before any of them reached a query.
	if !c.Source.Provides(c.Name) {
		w.fail(&ColumnError{Column: c.Name, Source: c.Source})
		return
	}
	w.ident(c.Source.Ref())
	w.b.WriteByte('.')
	w.ident(c.Name)
}

// node writes an expression. nested reports whether the node appears inside
// another boolean expression, which is the only thing parentheses depend on: a
// group at the top of a WHERE clause needs none, and one inside another group
// does.
func (w *writer) node(n Node, nested bool) {
	if w.err != nil {
		return
	}
	switch n := n.(type) {
	case Column:
		w.column(n)
	case Arg:
		w.arg(n.Value)
	case Bool:
		if n.Value {
			w.b.WriteString("TRUE")
		} else {
			w.b.WriteString("FALSE")
		}
	case Binary:
		w.binary(n)
	case Unary:
		w.unary(n, nested)
	case Group:
		w.group(n, nested)
	case In:
		w.in(n)
	case Between:
		w.between(n)
	case Raw:
		w.raw(n)
	case Exists:
		w.exists(n)
	case SubqueryValue:
		w.subqueryValue(n)
	case InSubquery:
		w.inSubquery(n)
	case Fail:
		w.fail(n.Err)
	case Null:
		w.b.WriteString("NULL")
	case Excluded:
		w.b.WriteString("EXCLUDED.")
		w.ident(n.Name)
	case Aggregate:
		w.aggregate(n)
	case Arith:
		w.arith(n, nested)
	case Case:
		w.writeCase(n)
	case Call:
		w.writeCall(n)
	case Cast:
		w.writeCast(n)
	case Extract:
		w.writeExtract(n)
	case Infix:
		w.writeInfix(n)
	case Prefix:
		w.writePrefix(n)
	case Quantified:
		w.writeQuantified(n)
	case RowValue:
		w.writeRowValue(n)
	default:
		w.fail(fmt.Errorf("cannot compile %T", n))
	}
}

// raw splices a caller's fragment into the statement, renumbering its
// placeholders into the surrounding parameter list.
//
// A local index appearing more than once binds one argument, not several, which
// is how PostgreSQL's own parameters behave: $1 twice in a statement is one
// parameter read twice.
func (w *writer) raw(n Raw) {
	refs, err := ScanPlaceholders(n.SQL)
	if err != nil {
		w.fail(fmt.Errorf("raw fragment: %w", err))
		return
	}
	if err := ValidatePlaceholders(refs, len(n.Args)); err != nil {
		w.fail(err)
		return
	}
	global := make(map[int]int, len(n.Args))
	w.b.WriteString(RewritePlaceholders(n.SQL, refs, func(local int) string {
		if g, ok := global[local]; ok {
			return "$" + strconv.Itoa(g)
		}
		w.args = append(w.args, n.Args[local-1])
		global[local] = len(w.args)
		return "$" + strconv.Itoa(len(w.args))
	}))
}

// exists writes a correlated subquery.
//
// No parentheses are added around it beyond the subquery's own: EXISTS (...) is
// already a single term, so it composes inside an AND or an OR without help,
// and NOT binds to the EXISTS rather than to whatever follows.
func (w *writer) exists(n Exists) {
	if n.Sub == nil {
		w.fail(fmt.Errorf("exists has no subquery"))
		return
	}
	if n.Not {
		w.b.WriteString("NOT ")
	}
	w.b.WriteString("EXISTS (")
	// The subquery pushes a frame of its own and pops it on the way out, so its
	// source is visible to the correlation and to nothing after it. The
	// enclosing frames stay visible, which is what makes the correlation legal.
	if err := n.Sub.write(w); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteByte(')')
}

func (w *writer) binary(n Binary) {
	op, ok := opSQL[n.Op]
	if !ok {
		w.fail(fmt.Errorf("cannot compile binary operator %d", n.Op))
		return
	}
	w.node(n.Left, true)
	w.b.WriteByte(' ')
	w.b.WriteString(op)
	w.b.WriteByte(' ')
	w.node(n.Right, true)
}

func (w *writer) unary(n Unary, nested bool) {
	switch n.Op {
	case OpNot:
		if nested {
			w.b.WriteByte('(')
		}
		w.b.WriteString("NOT ")
		w.node(n.X, true)
		if nested {
			w.b.WriteByte(')')
		}
	case OpIsNull, OpIsNotNull:
		w.node(n.X, true)
		w.b.WriteByte(' ')
		w.b.WriteString(opSQL[n.Op])
	default:
		w.fail(fmt.Errorf("cannot compile unary operator %d", n.Op))
	}
}

func (w *writer) group(n Group, nested bool) {
	op, ok := opSQL[n.Op]
	if !ok || (n.Op != OpAnd && n.Op != OpOr) {
		w.fail(fmt.Errorf("cannot compile group operator %d", n.Op))
		return
	}
	// A group is only ever built with two or more items — And and Or collapse
	// the shorter cases at the API boundary — but a hand-built empty group
	// would otherwise compile to nothing at all, which is worse than an error.
	if len(n.Items) == 0 {
		w.fail(fmt.Errorf("empty %s group", op))
		return
	}
	if nested {
		w.b.WriteByte('(')
	}
	for i, item := range n.Items {
		if i > 0 {
			w.b.WriteByte(' ')
			w.b.WriteString(op)
			w.b.WriteByte(' ')
		}
		w.node(item, true)
	}
	if nested {
		w.b.WriteByte(')')
	}
}

func (w *writer) in(n In) {
	// IN () is not valid PostgreSQL, and the caller who wrote In() over an
	// empty slice meant "nothing matches". FALSE says exactly that.
	if len(n.Values) == 0 {
		w.b.WriteString("FALSE")
		return
	}
	w.node(n.X, true)
	w.b.WriteString(" IN (")
	for i, v := range n.Values {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.node(v, true)
	}
	w.b.WriteByte(')')
}

func (w *writer) between(n Between) {
	w.node(n.X, true)
	w.b.WriteString(" BETWEEN ")
	w.node(n.Lo, true)
	w.b.WriteString(" AND ")
	w.node(n.Hi, true)
}

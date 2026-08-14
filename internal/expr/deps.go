package expr

// Walking the tree.
//
// Once a statement can be nested, "which sources does this expression need"
// stops being a question the writer can answer as it goes. A JOIN's ON clause
// may only name the items introduced before it, a non-LATERAL derived table may
// name none of its siblings at all, and a WITH item may only name the ones
// declared above it. Each of those is a question about an expression's
// dependencies asked at a point where the SQL has not been written yet.
//
// So dependencies are computed by walking, not stored. A node is immutable and
// its children are immutable, so the walk is a pure function of the tree: two
// walks of one expression report the same sources in the same order, which is
// what makes the errors deterministic. Storing the set on each node would mean
// every constructor maintaining it, and every constructor is a place it could
// be maintained wrongly.
//
// Every walk in this package goes through children, so a node type added later
// is handled everywhere by being added once. A node it does not know has no
// children, which is the safe answer for the two nodes that genuinely have none
// the tree can see: a Raw fragment is text nobody parsed, and an argument is a
// value.

// children visits a node's immediate subexpressions.
//
// A nested statement is not one of them. It is a scope of its own with its own
// sources and its own aggregates, and treating its interior as part of the
// enclosing expression is what would make a WHERE clause reject a perfectly
// legal correlated subquery for containing an aggregate.
func children(n Node, visit func(Node)) {
	switch n := n.(type) {
	case Binary:
		visit(n.Left)
		visit(n.Right)
	case Arith:
		visit(n.Left)
		visit(n.Right)
	case Unary:
		visit(n.X)
	case Group:
		for _, it := range n.Items {
			visit(it)
		}
	case In:
		visit(n.X)
		for _, v := range n.Values {
			visit(v)
		}
	case InSubquery:
		visit(n.X)
	case Between:
		visit(n.X)
		visit(n.Lo)
		visit(n.Hi)
	case Aggregate:
		for _, a := range n.Args {
			visit(a)
		}
		if n.Filter != nil {
			visit(n.Filter)
		}
		n.Over.walk(visit)
	case Case:
		for _, br := range n.When {
			visit(br.Cond)
			visit(br.Then)
		}
		if n.Else != nil {
			visit(n.Else)
		}
	case Call:
		for _, a := range n.Args {
			visit(a)
		}
	case Cast:
		visit(n.X)
	case Extract:
		visit(n.X)
	case Infix:
		visit(n.Left)
		visit(n.Right)
	case Prefix:
		visit(n.X)
	case Quantified:
		visit(n.Left)
		visit(n.Right)
	case RowValue:
		for _, it := range n.Items {
			visit(it)
		}
	}
}

// subqueryOf returns the statement a node nests, if it nests one.
func subqueryOf(n Node) Subquery {
	switch n := n.(type) {
	case Exists:
		return n.Sub
	case SubqueryValue:
		return n.Sub
	case InSubquery:
		return n.Sub
	default:
		return nil
	}
}

// walkSources reports every source a node refers to, in the order encountered.
//
// A nested statement contributes its free sources rather than all of them: the
// sources it introduces itself are its own business, and reporting them would
// make an uncorrelated subquery look like a dependency on tables the enclosing
// query has never heard of.
func walkSources(n Node, add func(*Source)) {
	if n == nil {
		return
	}
	if c, ok := n.(Column); ok && c.Source != nil {
		add(c.Source)
	}
	if sub := subqueryOf(n); sub != nil {
		sub.free(add)
	}
	children(n, func(child Node) { walkSources(child, add) })
}

// walkEvery reports every source a node refers to, descending into nested
// statements completely rather than only through their correlation.
//
// It answers a different question from walkSources: not "what does this
// expression need from outside" but "what does this expression mention at all".
// A WITH item that selects from an earlier WITH item binds that reference in
// its own FROM clause, so the dependency is invisible to a free-source walk and
// is exactly what the declaration-order check has to see.
func walkEvery(n Node, add func(*Source)) {
	if n == nil {
		return
	}
	if c, ok := n.(Column); ok && c.Source != nil {
		add(c.Source)
		if c.Source.kind == SourceDerived && c.Source.sub != nil {
			c.Source.sub.each(add)
		}
	}
	if sub := subqueryOf(n); sub != nil {
		sub.each(add)
	}
	children(n, func(child Node) { walkEvery(child, add) })
}

// sourcesOf collects a node's dependencies into a slice, de-duplicated by
// identity and in encounter order.
func sourcesOf(n Node) []*Source {
	seen := make(map[*Source]bool)
	var out []*Source
	walkSources(n, func(s *Source) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	})
	return out
}

// Holds reports whether a tree contains a node the predicate accepts, without
// descending into a nested statement.
//
// It is how the builder above this package answers the clause questions SQL
// asks: an aggregate cannot appear in a WHERE clause, a window function cannot
// appear in either a WHERE or a HAVING one, and both restrictions are about
// this statement rather than about the subqueries inside it.
func Holds(n Node, match func(Node) bool) bool {
	if n == nil {
		return false
	}
	if match(n) {
		return true
	}
	found := false
	children(n, func(child Node) {
		if !found {
			found = Holds(child, match)
		}
	})
	return found
}

// IsAggregate reports whether a node is a grouping aggregate.
//
// A windowed one is not. It computes over a frame and leaves the rows alone, so
// it belongs where a value belongs rather than where a group does — which is
// exactly the distinction the WHERE and HAVING checks above this package are
// asking about.
func IsAggregate(n Node) bool {
	a, ok := n.(Aggregate)
	return ok && a.Over == nil
}

// ResultArity reports how many columns a nested statement returns, or -1 when
// its terms disagree.
func ResultArity(s Subquery) int {
	if s == nil {
		return -1
	}
	return s.resultArity()
}

// ExistsProbe reduces a statement to the rows it matches, which is all an
// EXISTS reads. It is the same canonicalisation the relation planner's
// semi-joins use, shared rather than repeated.
func ExistsProbe(sel *Select) *Select { return probe(sel) }

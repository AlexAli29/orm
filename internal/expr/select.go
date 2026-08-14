package expr

import (
	"fmt"
	"strconv"
)

// Order is one ORDER BY term.
//
// M3 ordered by a column and a direction, and nothing else. M11 needs more, but
// only in two places — a window's own ordering and the ORDER BY that DISTINCT
// ON is defined against — so Expr is an alternative to Column rather than a
// replacement for it. An entity ordering is still a column, which is what lets
// the relation planner re-express one against another occurrence of the same
// table without re-resolving an expression.
type Order struct {
	Column Column
	// Expr, when set, is ordered by instead of Column.
	Expr Node
	Desc bool
}

// node returns the term's expression.
func (o Order) node() Node {
	if o.Expr != nil {
		return o.Expr
	}
	return o.Column
}

// writeOrderTerms renders a comma-separated ORDER BY list.
func (w *writer) writeOrderTerms(orders []Order) {
	for i, o := range orders {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.node(o.node(), false)
		if o.Desc {
			w.b.WriteString(" DESC")
		} else {
			w.b.WriteString(" ASC")
		}
	}
}

// Select is a SELECT over one root source, optionally with others joined to it.
type Select struct {
	// With holds the statement's WITH items, in declaration order. They are
	// written before anything else, so their parameters are numbered first.
	With  []*Source
	From  *Source
	Joins []Join
	// Columns is the entity select list: one mapped column per entry, in
	// metadata order. It is what a query over an entity selects.
	Columns []Column
	// Items is the projection select list. A statement uses one list or the
	// other — an entity query knows its columns, a projection knows its
	// expressions — and Items wins when both are set, because only a
	// projection sets it.
	Items    []SelectItem
	Distinct bool
	// DistinctOn holds the expressions of PostgreSQL's DISTINCT ON, which
	// keeps the first row of each group of equal values rather than removing
	// duplicate whole rows. It is a different clause from Distinct and the two
	// cannot both be set.
	DistinctOn []Node
	Where      Node
	// GroupBy and Having are the grouped form. Group order is the caller's and
	// is never sorted: it decides the grouping PostgreSQL performs.
	GroupBy []Node
	Having  Node
	OrderBy []Order
	// Limit and Offset are applied when non-nil. A limit of zero is a legal
	// query that returns nothing; only a negative one is a mistake.
	Limit  *int
	Offset *int
	// ForUpdate locks the rows the statement returns. It is the M5 spelling,
	// kept because the relation planner and every existing caller use it, and
	// it means FOR UPDATE with PostgreSQL's default waiting policy.
	ForUpdate bool
	// Lock is the richer form: a strength, a waiting policy and the sources to
	// lock. When both are set this one wins, because only a caller who asked
	// for a strength sets it.
	Lock Lock
	// SelectOne makes the statement select the constant 1 rather than its
	// columns. A count or an existence test needs a row per match and no
	// values from it, and asking for the columns anyway would make the server
	// fetch and decode data nobody reads.
	SelectOne bool
}

// clone returns a copy that can be adjusted without disturbing the original.
// The slices are shared because nothing here rewrites their elements.
func (s *Select) clone() *Select {
	out := *s
	return &out
}

// Compile renders the statement and its parameters.
//
// The returned SQL contains no value from the caller — every one of them is in
// args, in placeholder order.
func (s *Select) Compile() (string, []any, error) {
	w := &writer{}
	if err := s.write(w); err != nil {
		return "", nil, err
	}
	if w.err != nil {
		return "", nil, w.err
	}
	return w.b.String(), w.args, nil
}

// write renders the statement into an existing writer, so that a statement
// wrapping this one shares its scope and its parameter numbering.
func (s *Select) write(w *writer) error {
	switch {
	case s.From == nil:
		return fmt.Errorf("select has no source table")
	case len(s.Columns) == 0 && len(s.Items) == 0 && !s.SelectOne:
		return fmt.Errorf("select has no columns")
	case s.Limit != nil && *s.Limit < 0:
		return fmt.Errorf("negative limit %d", *s.Limit)
	case s.Offset != nil && *s.Offset < 0:
		return fmt.Errorf("negative offset %d", *s.Offset)
	case s.Distinct && len(s.DistinctOn) > 0:
		return fmt.Errorf("a statement is DISTINCT or DISTINCT ON, not both")
	}

	if w.scope.Depth() >= MaxDepth {
		return fmt.Errorf("statements are nested more than %d deep, which is past the point where a query is a mistake rather than a request", MaxDepth)
	}
	// The frame is popped on the way out, so a statement nested inside another
	// — a correlated EXISTS, or the subquery of a LATERAL join — sees its own
	// sources and its enclosing ones while it is being written, and neither
	// after.
	w.scope.Push()
	defer w.scope.Pop()

	// The WITH clause is written while the frame is still empty, which is not
	// an optimisation: a WITH item cannot name the FROM items of the statement
	// that declares it, and writing it first is what makes that structural
	// rather than a rule somebody has to remember.
	popWith, err := w.writeWith(s.With)
	defer popWith()
	if err != nil {
		return err
	}
	if err := s.enterSources(w); err != nil {
		return err
	}
	if err := s.validateNullability(); err != nil {
		return err
	}

	w.b.WriteString("SELECT ")
	switch {
	case s.SelectOne:
	case s.Distinct:
		w.b.WriteString("DISTINCT ")
	case len(s.DistinctOn) > 0:
		w.b.WriteString("DISTINCT ON (")
		for i, d := range s.DistinctOn {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.node(d, false)
		}
		w.b.WriteString(") ")
	}
	switch {
	case s.SelectOne:
		w.b.WriteString("1")
	case len(s.Items) > 0:
		w.selectList(s.Items)
	default:
		for i, c := range s.Columns {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.column(c)
		}
	}

	w.b.WriteString(" FROM ")
	w.source(s.From)

	for _, j := range s.Joins {
		w.writeJoin(j)
	}

	// A WHERE of constant TRUE restricts nothing, so it is dropped rather than
	// emitted. That is what makes And() over an empty predicate slice produce a
	// query with no WHERE clause at all, which is the shape dynamic filtering
	// needs.
	if s.Where != nil && !IsTrue(s.Where) {
		w.b.WriteString(" WHERE ")
		w.node(s.Where, false)
	}

	// GROUP BY precedes HAVING, and both precede ORDER BY. PostgreSQL fixes
	// the order; writing them in another is a syntax error rather than a
	// style.
	if len(s.GroupBy) > 0 {
		w.b.WriteString(" GROUP BY ")
		for i, g := range s.GroupBy {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.node(g, false)
		}
	}
	if s.Having != nil && !IsTrue(s.Having) {
		w.b.WriteString(" HAVING ")
		w.node(s.Having, false)
	}

	if len(s.OrderBy) > 0 {
		w.b.WriteString(" ORDER BY ")
		w.writeOrderTerms(s.OrderBy)
	}

	// PostgreSQL fixes this order: LIMIT, then OFFSET, then the locking
	// clause. Writing them in any other order is a syntax error, not a style.
	if s.Limit != nil {
		w.b.WriteString(" LIMIT ")
		w.b.WriteString(strconv.Itoa(*s.Limit))
	}
	if s.Offset != nil {
		w.b.WriteString(" OFFSET ")
		w.b.WriteString(strconv.Itoa(*s.Offset))
	}
	switch {
	case s.Lock.Strength != LockNone:
		w.writeLock(s.Lock, &w.scope)
	case s.ForUpdate:
		w.b.WriteString(" FOR UPDATE")
		// PostgreSQL refuses to lock the nullable side of an outer join, and
		// locking a relation the caller only asked to read would be a lock
		// they never requested. Naming the root table does both jobs: the
		// statement is legal and only the rows it is about are locked.
		if len(s.Joins) > 0 {
			w.b.WriteString(" OF ")
			w.ident(s.From.Ref())
		}
	}
	return nil
}

// enterSources brings the FROM items into scope in the order the FROM clause
// introduces them, checking each against what is visible at that point.
//
// Doing it in order is the whole point. SQL's own rule is sequential: a join
// condition may name the items to its left and the one it is attaching, and
// nothing further right. Validating against the finished set of sources instead
// would accept
//
//	FROM a JOIN b ON b.x = c.x JOIN c ON ...
//
// which PostgreSQL refuses, and refuses with a message about column c.x rather
// than about the order the joins were written in.
//
// The select list is a different matter: it is written after this loop and sees
// every item, which is why the frame is complete before any of it is rendered.
func (s *Select) enterSources(w *writer) error {
	items := make([]Join, 0, len(s.Joins)+1)
	items = append(items, Join{Source: s.From})
	items = append(items, s.Joins...)

	// A FROM item's own siblings are what LATERAL is about, so they have to be
	// known before the first of them is checked.
	level := make(map[*Source]bool, len(items))
	for _, it := range items {
		if it.Source != nil {
			level[it.Source] = true
		}
	}

	for i, it := range items {
		if it.Source == nil {
			return fmt.Errorf("a statement selects from no source")
		}
		if err := checkCorrelation(w, it, level); err != nil {
			return err
		}
		if err := w.scope.Add(it.Source); err != nil {
			return err
		}
		if i == 0 || it.On == nil {
			continue
		}
		if err := checkVisible(w, it.On); err != nil {
			return fmt.Errorf("in the ON condition of %s %s: %w", it.Kind, it.Source, err)
		}
	}
	return nil
}

// checkCorrelation refuses a FROM subquery that names something it may not.
//
// A plain derived table is evaluated once, independently of the items beside
// it, so naming one of them is not a query PostgreSQL will run — it is a
// reference to a relation that does not exist at that point. LATERAL is exactly
// the permission to do it, and even then only leftwards: a lateral subquery
// sees the items introduced before it and not the ones after.
//
// References that leave this query level entirely are a different question and
// are left to the scope, which knows the enclosing frames and this function
// does not.
func checkCorrelation(w *writer, it Join, level map[*Source]bool) error {
	if it.Source.kind != SourceDerived || it.Source.sub == nil {
		return nil
	}
	var bad *Source
	it.Source.sub.free(func(s *Source) {
		if bad == nil && level[s] && s != it.Source {
			bad = s
		}
	})
	if bad == nil {
		return nil
	}
	if !it.Lateral {
		return fmt.Errorf("derived table %q refers to %s, which is a source of the same query; a subquery in a FROM clause can only do that when it is attached with LATERAL",
			it.Source.Ref(), bad)
	}
	if !w.scope.Visible(bad) {
		return fmt.Errorf("lateral source %q refers to %s, which the FROM clause introduces after it; a lateral subquery sees the sources written before it and no others",
			it.Source.Ref(), bad)
	}
	return nil
}

// checkVisible reports the first source a node names that is not in scope.
//
// The writer checks this too, when it comes to render the column. Checking it
// here as well is what makes the sequential rule enforceable: by the time the
// ON clause is written every item is in scope, so the writer's check would pass
// on a condition that names a table joined later.
func checkVisible(w *writer, n Node) error {
	for _, src := range sourcesOf(n) {
		if !w.scope.Visible(src) {
			return &ScopeError{Source: src, Visible: w.scope.Sources()}
		}
	}
	return nil
}

// The statement interface a nested SELECT satisfies.

func (s *Select) bound(add func(*Source)) {
	if s.From != nil {
		add(s.From)
	}
	for _, j := range s.Joins {
		if j.Source != nil {
			add(j.Source)
		}
	}
}

// free reports the sources this statement names and does not introduce.
//
// That set is its correlation, and it is what decides whether a subquery may
// stand in a FROM clause without LATERAL, whether a WITH item is legal where it
// was declared, and what an enclosing scope has to make visible.
func (s *Select) free(add func(*Source)) {
	own := make(map[*Source]bool)
	s.bound(func(src *Source) { own[src] = true })
	outer := func(src *Source) {
		if !own[src] {
			add(src)
		}
	}
	// A derived source this statement binds still carries a statement of its
	// own, and that statement's correlation escapes to this level: it is what
	// a LATERAL item refers to, and it does not stop being a dependency
	// because the item using it is bound here.
	for _, src := range s.sources() {
		if src.kind == SourceDerived && src.sub != nil {
			src.sub.free(outer)
		}
	}
	s.walk(func(n Node) { walkSources(n, outer) })
	for _, c := range s.With {
		if c != nil && c.sub != nil {
			c.sub.free(outer)
		}
	}
}

func (s *Select) each(add func(*Source)) {
	for _, src := range s.sources() {
		add(src)
		if src.kind == SourceDerived && src.sub != nil {
			src.sub.each(add)
		}
	}
	s.walk(func(n Node) { walkEvery(n, add) })
	for _, c := range s.With {
		if c != nil && c.sub != nil {
			c.sub.each(add)
		}
	}
}

// sources lists the FROM items in clause order.
func (s *Select) sources() []*Source {
	out := make([]*Source, 0, len(s.Joins)+1)
	if s.From != nil {
		out = append(out, s.From)
	}
	for _, j := range s.Joins {
		if j.Source != nil {
			out = append(out, j.Source)
		}
	}
	return out
}

// walk visits every expression the statement holds, in the order the compiler
// renders them. Keeping the two in one order is what makes a dependency walk
// and a placeholder walk agree about what the statement contains.
func (s *Select) walk(visit func(Node)) {
	for _, d := range s.DistinctOn {
		visit(d)
	}
	for _, it := range s.Items {
		visit(it.Node)
	}
	for _, c := range s.Columns {
		visit(c)
	}
	for _, j := range s.Joins {
		if j.On != nil {
			visit(j.On)
		}
	}
	if s.Where != nil {
		visit(s.Where)
	}
	for _, g := range s.GroupBy {
		visit(g)
	}
	if s.Having != nil {
		visit(s.Having)
	}
	for _, o := range s.OrderBy {
		visit(o.node())
	}
}

// resultArity reports how many columns the statement returns.
func (s *Select) resultArity() int {
	switch {
	case s.SelectOne:
		return 1
	case len(s.Items) > 0:
		return len(s.Items)
	default:
		return len(s.Columns)
	}
}

// countAlias names the subquery a count wraps. It carries the reserved prefix
// so that it cannot collide with an alias a caller chose.
const countAlias = "_orm_count"

// CountOf counts the rows an inner statement returns.
//
// It wraps rather than rewrites, because LIMIT and OFFSET apply to the rows
// selected and a bare count(*) with a LIMIT would count the limit away — the
// query would report the whole table. Wrapping keeps "how many rows would All
// return" exactly true.
type CountOf struct {
	Inner *Select
}

// Compile renders SELECT count(*) FROM ( inner ) AS _orm_count.
func (c *CountOf) Compile() (string, []any, error) {
	if c.Inner == nil {
		return "", nil, fmt.Errorf("count has no inner statement")
	}
	w := &writer{}
	w.b.WriteString("SELECT count(*) FROM (")
	if err := c.Inner.write(w); err != nil {
		return "", nil, err
	}
	w.b.WriteString(") AS ")
	w.ident(countAlias)
	if w.err != nil {
		return "", nil, w.err
	}
	return w.b.String(), w.args, nil
}

// ExistsOf reports whether an inner statement returns any row.
type ExistsOf struct {
	Inner *Select
}

// Compile renders SELECT EXISTS ( inner ).
func (e *ExistsOf) Compile() (string, []any, error) {
	if e.Inner == nil {
		return "", nil, fmt.Errorf("exists has no inner statement")
	}
	w := &writer{}
	w.b.WriteString("SELECT EXISTS (")
	if err := e.Inner.write(w); err != nil {
		return "", nil, err
	}
	w.b.WriteString(")")
	if w.err != nil {
		return "", nil, w.err
	}
	return w.b.String(), w.args, nil
}

// CountFrom returns the statement that counts what sel would return.
func CountFrom(sel *Select) *CountOf { return &CountOf{Inner: probe(sel)} }

// ExistsFrom returns the statement that reports whether sel returns anything.
func ExistsFrom(sel *Select) *ExistsOf { return &ExistsOf{Inner: probe(sel)} }

// probe reduces a statement to the rows it selects.
//
// The columns go, because neither counting nor existence reads a value. The
// ordering goes with them: it cannot change how many rows there are, and
// sorting rows nobody looks at is work the server would do for nothing.
func probe(sel *Select) *Select {
	inner := sel.clone()
	inner.OrderBy = nil
	// A grouped statement's rows are its groups, and a group is only defined
	// by what the select list computes. Reducing it to the constant 1 would
	// still produce one row per group, which is what a count of it means — but
	// a DISTINCT over the select list would not survive, so that one keeps its
	// items.
	if inner.Distinct {
		return inner
	}
	inner.SelectOne = true
	inner.Columns = nil
	inner.Items = nil
	return inner
}

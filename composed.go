package orm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// The composed query.
//
// An entity query answers "which rows of this table" and a projection query
// answers it with a different result shape. A composed query answers a question
// neither can be asked: which rows of a statement built from several sources —
// tables, subqueries, CTEs — related by joins the caller wrote.
//
// It shares everything with the two of them that can be shared. The same
// predicates, the same aggregates, the same result shapes, the same compiler,
// the same scope. What differs is the tag its expressions carry ([Composed])
// and that its FROM clause is a list rather than a table.

// NoResult is the result type of a query built to be a source rather than to
// be read.
//
// A derived table, a CTE body and a recursive term all produce rows that some
// enclosing statement reads; nothing scans them directly. Giving them a result
// type at all would mean writing a scanner nobody calls, so they have this one
// instead, and every terminal operation on such a query refuses.
type NoResult struct{}

// Term is a row-producing statement that is a plain SELECT.
//
// It is satisfied by every query this package builds that renders one — entity,
// projection and composed — and deliberately not by a set operation, which
// renders a compound instead.
//
// It is the narrowest thing that [Exists] and a recursive CTE can take, because
// both do more than nest what they are given: EXISTS canonicalises the statement
// into the rows it matches, and a recursive CTE compares the anchor's select
// list against the recursive term's. Neither question can be asked of a
// statement that is not a Select.
//
// The other two capabilities are separate interfaces rather than this one
// widened. [SourceTerm] is what a derived table or a CTE body takes, and
// [ValueTerm] is what a membership test or a scalar value takes; both accept a
// compound, because neither needs to look inside the statement it nests.
type Term interface {
	// term renders the statement's tree. It is unexported so that the set of
	// things that can be nested stays the set this package can compile.
	term() (*expr.Select, error)
}

// ComposedQuery builds a SELECT over sources the caller composed.
//
// Its methods mutate the builder and return it, and mistakes are recorded
// rather than raised — the same contract [Query] and [SelectQuery] have. A
// ComposedQuery is mutable and is not safe for concurrent use;
// [ComposedQuery.Clone] is what makes one builder the base for several.
type ComposedQuery[R any] struct {
	ex      Executor
	items   []expr.SelectItem
	newScan func() func(pgxRows) (R, error)
	// shape is the projection's result description, kept so that a composed
	// query can be a branch of a set operation without anything being
	// reconstructed from the statement it renders.
	shape resultShape

	with       []*Source
	from       *Source
	joins      []expr.Join
	wheres     []Predicate[Composed]
	groupBy    []expr.Node
	havings    []Predicate[Composed]
	orderBy    []Order[Composed]
	distinct   bool
	distinctOn []expr.Node
	limit      *int
	offset     *int
	forUpdate  bool
	lock       expr.Lock
	errs       []error
}

// Compose starts a composed query returning a result shape.
//
//	orm.Compose(db.Executor(), shape).
//	    From(Users.Source()).
//	    LeftJoin(Profiles.Source(), orm.Of(Profiles.UserID).EqCol(orm.Of(Users.ID))).
//	    All(ctx)
//
// The shape is an ordinary [Projection] instantiated at [Composed], so every
// ProjectN constructor builds one and the scanning is M10's: typed locals, one
// Scan, no reflection.
func Compose[R any](ex Executor, p Projection[Composed, R]) *ComposedQuery[R] {
	q := &ComposedQuery[R]{ex: ex, items: slices.Clone(p.items), newScan: p.newScan, shape: p.shape}
	if err := p.validate(); err != nil {
		q.fail(err)
	}
	return q
}

// Rows starts a query built to be a source rather than to be read.
//
// The declarations are its select list and its output columns at once, so a
// derived table or a CTE built from it names exactly what it selects and
// nothing has to be kept in step by hand:
//
//	userID := orm.Named("user_id", orm.Of(Posts.AuthorID))
//	count  := orm.Named("post_count", orm.Count[orm.Composed]())
//
//	stats := orm.Sub("post_stats", orm.Rows(userID, count).
//	    From(Posts.Source()).
//	    GroupBy(orm.Of(Posts.AuthorID)))
func Rows(outs ...Output) *ComposedQuery[NoResult] {
	q := &ComposedQuery[NoResult]{}
	if len(outs) == 0 {
		q.fail(errors.New("a row source selects nothing"))
	}
	seen := make(map[string]bool, len(outs))
	for i, o := range outs {
		switch {
		case o == nil:
			q.fail(fmt.Errorf("output %d is missing", i+1))
			continue
		case o.outName() == "":
			q.fail(fmt.Errorf("output %d has no name", i+1))
			continue
		case seen[o.outName()]:
			q.fail(fmt.Errorf("two outputs are named %q; a column of a row source is addressed by name, so the second would be unreachable", o.outName()))
			continue
		}
		seen[o.outName()] = true
		q.items = append(q.items, o.outItem())
	}
	return q
}

func (q *ComposedQuery[R]) fail(err error) {
	if err != nil {
		q.errs = append(q.errs, err)
	}
}

// Clone returns an independent copy. Every slice the builder owns is copied,
// and the numbers behind limit and offset are copied rather than shared. The
// sources are not: a source is immutable, and two builders naming one
// occurrence is what an alias means.
func (q *ComposedQuery[R]) Clone() *ComposedQuery[R] {
	out := &ComposedQuery[R]{
		ex:         q.ex,
		items:      slices.Clone(q.items),
		newScan:    q.newScan,
		with:       slices.Clone(q.with),
		from:       q.from,
		joins:      slices.Clone(q.joins),
		wheres:     slices.Clone(q.wheres),
		groupBy:    slices.Clone(q.groupBy),
		havings:    slices.Clone(q.havings),
		orderBy:    slices.Clone(q.orderBy),
		distinct:   q.distinct,
		distinctOn: slices.Clone(q.distinctOn),
		forUpdate:  q.forUpdate,
		lock:       q.lock,
		errs:       slices.Clone(q.errs),
	}
	if q.limit != nil {
		n := *q.limit
		out.limit = &n
	}
	if q.offset != nil {
		n := *q.offset
		out.offset = &n
	}
	return out
}

// Using binds an executor, which is how a query built without one is run.
func (q *ComposedQuery[R]) Using(ex Executor) *ComposedQuery[R] {
	q.ex = ex
	return q
}

// From sets the first source of the FROM clause.
func (q *ComposedQuery[R]) From(src *Source) *ComposedQuery[R] {
	switch {
	case src == nil:
		q.fail(errors.New("From was given no source"))
	case q.from != nil:
		q.fail(errors.New("From was called twice; a further source is attached with a join"))
	default:
		q.from = src
	}
	return q
}

// Joins.
//
// A join attaches a source to the ones already introduced, and the condition
// may name those and the source being attached — nothing further along. That
// rule is sequential, PostgreSQL's, and enforced structurally: a condition
// naming a source joined later is refused when the statement is built, with a
// message about the order rather than about a column that does not exist.
//
// The target is any [Source]: a table occurrence, an alias of one, a derived
// table, a CTE reference. There is one join implementation because there is one
// kind of thing to join.
//
// # Nullability
//
// An outer join can produce a row in which one side is absent, and every value
// read from that side is then NULL whatever the column's constraint says. The
// select list has to say so — [Opt] and [OptRef] are how — and a statement that
// does not is refused rather than left to fail as a scan error. Nothing about
// the descriptors changes: the original column still means what it did in every
// other query.

// Join attaches a source with an INNER JOIN. Neither side becomes nullable.
func (q *ComposedQuery[R]) Join(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinInner, src, false, on)
}

// LeftJoin attaches a source with a LEFT JOIN, which can leave it with no row.
//
// Every value read from the attached source is nullable in this statement. Read
// them with [Opt] or [OptRef]; the compiler refuses a select list that does
// not, because the alternative is a scan error on the first parent with no
// child.
func (q *ComposedQuery[R]) LeftJoin(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinLeft, src, false, on)
}

// RightJoin attaches a source with a RIGHT JOIN, which can leave the sources
// already introduced with no row. Those become nullable; the attached one keeps
// the nullability it had.
func (q *ComposedQuery[R]) RightJoin(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinRight, src, false, on)
}

// FullJoin attaches a source with a FULL JOIN, which can leave either side with
// no row. Both become nullable.
func (q *ComposedQuery[R]) FullJoin(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinFull, src, false, on)
}

// CrossJoin attaches a source with a CROSS JOIN, which has no condition.
func (q *ComposedQuery[R]) CrossJoin(src *Source) *ComposedQuery[R] {
	return q.join(expr.JoinCross, src, false, nil)
}

// JoinLateral attaches a subquery that may name the sources introduced before
// it, with an INNER JOIN.
func (q *ComposedQuery[R]) JoinLateral(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinInner, src, true, on)
}

// LeftJoinLateral attaches a subquery that may name the sources introduced
// before it, with a LEFT JOIN.
//
// This is the shape PostgreSQL is unusually good at: a subquery evaluated once
// per row of the sources to its left, ordered and limited on its own terms.
// With no condition the join is ON TRUE, which is what such a subquery almost
// always wants — it has already said which rows it matches.
//
//	latest := orm.Sub("latest", orm.Rows(id).
//	    From(Posts.Source()).
//	    Where(orm.Eq(Posts.AuthorID, Users.ID)).
//	    OrderBy(orm.Of(Posts.CreatedAt).Desc()).
//	    Limit(1))
//
//	q.From(Users.Source()).LeftJoinLateral(latest)
func (q *ComposedQuery[R]) LeftJoinLateral(src *Source, on ...Predicate[Composed]) *ComposedQuery[R] {
	return q.join(expr.JoinLeft, src, true, on)
}

// CrossJoinLateral attaches a subquery that may name the sources introduced
// before it, with a CROSS JOIN — which keeps only the rows it matches.
func (q *ComposedQuery[R]) CrossJoinLateral(src *Source) *ComposedQuery[R] {
	return q.join(expr.JoinCross, src, true, nil)
}

func (q *ComposedQuery[R]) join(kind expr.JoinKind, src *Source, lateral bool, on []Predicate[Composed]) *ComposedQuery[R] {
	if src == nil {
		q.fail(errors.New("a join was given no source"))
		return q
	}
	// PostgreSQL's grammar allows LATERAL before a sub-SELECT and nothing else
	// this package builds. Attaching it to a table or a CTE reference is a
	// syntax error the server reports as "syntax error at end of input", which
	// says nothing about what was wrong.
	if lateral && !src.IsDerived() {
		q.fail(fmt.Errorf("LATERAL applies to a subquery, and %s is not one;"+
			" a source that has to see the rows beside it is built with Sub", src))
		return q
	}
	j := expr.Join{Kind: kind, Source: src, Lateral: lateral}
	if kind == expr.JoinCross {
		// PostgreSQL's grammar has no ON for a CROSS JOIN, so a condition is
		// not something to drop quietly: the caller meant a different join.
		if len(on) > 0 {
			q.fail(errors.New("a CROSS JOIN has no condition; an INNER JOIN is the one that takes an ON clause"))
			return q
		}
		q.joins = append(q.joins, j)
		return q
	}
	cond := And(on...)
	if err := cond.Err(); err != nil {
		q.fail(err)
		return q
	}
	if containsAggregate(cond.node) {
		q.fail(errors.New("a join condition cannot contain an aggregate; the rows are joined before any group exists"))
		return q
	}
	if containsWindow(cond.node) {
		q.fail(errWindowIn("a join condition"))
		return q
	}
	j.On = cond.node
	q.joins = append(q.joins, j)
	return q
}

// Where adds conditions, joined with AND.
func (q *ComposedQuery[R]) Where(ps ...Predicate[Composed]) *ComposedQuery[R] {
	for _, p := range ps {
		if containsAggregate(p.node) {
			q.fail(errors.New("Where cannot contain an aggregate; a condition over a group belongs in Having"))
			continue
		}
		if containsWindow(p.node) {
			q.fail(errWindowIn("a WHERE clause"))
			continue
		}
		q.wheres = append(q.wheres, p)
	}
	return q
}

// GroupBy groups the rows the query returns, in the order given.
func (q *ComposedQuery[R]) GroupBy(gs ...Grouping[Composed]) *ComposedQuery[R] {
	for _, g := range gs {
		node := g.groupNode()
		if containsAggregate(node) {
			q.fail(errors.New("GroupBy cannot contain an aggregate; a group is defined by the values it groups on"))
			continue
		}
		if containsWindow(node) {
			q.fail(errWindowIn("a GROUP BY clause"))
			continue
		}
		q.groupBy = append(q.groupBy, node)
	}
	return q
}

// Having filters the groups, after grouping.
//
// A window function is refused here as well as in Where. PostgreSQL computes
// windows after HAVING has already chosen the groups, so a condition on one
// cannot be part of the same statement — it belongs outside a derived table
// that computes it.
func (q *ComposedQuery[R]) Having(ps ...Predicate[Composed]) *ComposedQuery[R] {
	for _, p := range ps {
		if containsWindow(p.node) {
			q.fail(errWindowIn("a HAVING clause"))
			continue
		}
		q.havings = append(q.havings, p)
	}
	return q
}

// OrderBy adds ordering terms, in the order given.
func (q *ComposedQuery[R]) OrderBy(os ...Order[Composed]) *ComposedQuery[R] {
	q.orderBy = append(q.orderBy, os...)
	return q
}

// Limit caps the rows returned. A limit of zero returns nothing.
func (q *ComposedQuery[R]) Limit(n int) *ComposedQuery[R] {
	if n < 0 {
		q.fail(fmt.Errorf("negative limit %d", n))
		return q
	}
	q.limit = &n
	return q
}

// Offset skips rows before returning any.
func (q *ComposedQuery[R]) Offset(n int) *ComposedQuery[R] {
	if n < 0 {
		q.fail(fmt.Errorf("negative offset %d", n))
		return q
	}
	q.offset = &n
	return q
}

// Distinct removes duplicate rows, comparing the whole select list.
func (q *ComposedQuery[R]) Distinct() *ComposedQuery[R] {
	q.distinct = true
	return q
}

// DistinctOn keeps the first row of each group of equal expressions.
//
// It is PostgreSQL's, and it is a different clause from [ComposedQuery.Distinct]:
// that one compares whole rows, this one compares the expressions named here
// and keeps one row per distinct combination. Which row that is depends on the
// ORDER BY, whose leading terms PostgreSQL requires to match these expressions:
//
//	q.DistinctOn(orm.Of(Posts.AuthorID)).
//	    OrderBy(orm.Of(Posts.AuthorID).Asc(), orm.Of(Posts.CreatedAt).Desc())
//
// The requirement is not checked here. Deciding whether two expressions are the
// same one is a judgement about SQL equivalence this package cannot make
// soundly, and an over-strict version would refuse queries PostgreSQL runs
// happily — so the server keeps that job, and it names the clause when it
// complains.
func (q *ComposedQuery[R]) DistinctOn(gs ...Grouping[Composed]) *ComposedQuery[R] {
	for _, g := range gs {
		node := g.groupNode()
		if containsAggregate(node) {
			q.fail(errors.New("DistinctOn cannot contain an aggregate; it chooses among rows, which happens before any group exists"))
			continue
		}
		if containsWindow(node) {
			q.fail(errWindowIn("a DISTINCT ON clause"))
			continue
		}
		q.distinctOn = append(q.distinctOn, node)
	}
	return q
}

// ForUpdate locks the rows the statement returns.
func (q *ComposedQuery[R]) ForUpdate() *ComposedQuery[R] {
	q.forUpdate = true
	return q
}

// SQL renders the statement and its parameters without running it.
func (q *ComposedQuery[R]) SQL() (string, []any, error) {
	sel, err := q.build()
	if err != nil {
		return "", nil, err
	}
	return sel.Compile()
}

// term makes a composed query usable inside another statement.
func (q *ComposedQuery[R]) term() (*expr.Select, error) { return q.build() }

// outputs reports the select list's result names, which is what a derived
// table or a CTE built from this query provides.
func (q *ComposedQuery[R]) outputs() []string {
	out := make([]string, 0, len(q.items))
	for _, it := range q.items {
		out = append(out, it.Alias)
	}
	return out
}

// sourceTerm makes a composed query a row source.
//
// The statement is returned as an expr.Subquery, which is what a derived table
// and a WITH item hold. Nothing widens: a composed query is still a Select, and
// still a Term for the callers that need one.
func (q *ComposedQuery[R]) sourceTerm() (expr.Subquery, []string, error) {
	sel, err := q.build()
	if err != nil {
		return nil, nil, err
	}
	return sel, q.outputs(), nil
}

// valueTerm makes a composed query usable where a value is expected.
//
// The arity is the length of the select list, which is what the statement
// selects and what the projection — when there is one — describes. Those two
// cannot disagree: a Projection is refused unless it has one result slot per
// expression, so the select list is the shape's arity wherever a shape exists,
// and it is the only arity there is where one does not.
func (q *ComposedQuery[R]) valueTerm() (expr.Subquery, int, error) {
	sel, err := q.build()
	if err != nil {
		return nil, 0, err
	}
	return sel, len(q.items), nil
}

// build assembles the statement, refusing before any of it if the builder
// recorded a mistake.
func (q *ComposedQuery[R]) build() (*expr.Select, error) {
	if len(q.errs) > 0 {
		return nil, errors.Join(q.errs...)
	}
	if q.from == nil {
		return nil, errors.New("a composed query selects from no source; call From")
	}
	sel := &expr.Select{
		With:       slices.Clone(q.with),
		From:       q.from,
		Joins:      slices.Clone(q.joins),
		Items:      slices.Clone(q.items),
		Distinct:   q.distinct,
		DistinctOn: slices.Clone(q.distinctOn),
		GroupBy:    slices.Clone(q.groupBy),
		Limit:      q.limit,
		Offset:     q.offset,
		ForUpdate:  q.forUpdate,
		Lock:       q.lock,
	}
	where, having := And(q.wheres...), And(q.havings...)
	// A mistake inside a predicate travels with it, so it is collected here
	// rather than dropped along with the condition it belongs to.
	if err := errors.Join(where.Err(), having.Err()); err != nil {
		return nil, err
	}
	if !expr.IsTrue(where.node) {
		sel.Where = where.node
	}
	if !expr.IsTrue(having.node) {
		sel.Having = having.node
	}
	for _, o := range q.orderBy {
		if o.IsZero() {
			continue
		}
		sel.OrderBy = append(sel.OrderBy, o.order)
	}
	return sel, nil
}

// exec compiles and runs a statement.
func (q *ComposedQuery[R]) exec(ctx context.Context, stmt expr.Statement) (pgxRows, error) {
	if q.newScan == nil {
		return nil, errors.New("this query was built to be a source and has no result shape; build it with Compose to read it")
	}
	sql, args, err := stmt.Compile()
	if err != nil {
		return nil, err
	}
	if q.ex == nil {
		return nil, errors.New("the composed query has no executor")
	}
	rows, err := q.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("running the composed query: %w", err)
	}
	return rows, nil
}

// All runs the query and returns every row.
func (q *ComposedQuery[R]) All(ctx context.Context) ([]R, error) {
	sel, err := q.build()
	if err != nil {
		return nil, err
	}
	rows, err := q.exec(ctx, sel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scan := q.newScan()
	var out []R
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning the composed query: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the composed query: %w", err)
	}
	return out, nil
}

// One runs the query and returns the single row it selects, reporting
// [ErrNotFound] when there was none and [ErrMultipleRows] when there was more
// than one.
func (q *ComposedQuery[R]) One(ctx context.Context) (R, error) {
	var zero R

	sel, err := q.build()
	if err != nil {
		return zero, err
	}
	if sel.Limit == nil || *sel.Limit > 2 {
		two := 2
		sel.Limit = &two
	}
	rows, err := q.exec(ctx, sel)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, fmt.Errorf("reading the composed query: %w", err)
		}
		return zero, ErrNotFound
	}
	out, err := q.newScan()(rows)
	if err != nil {
		return zero, fmt.Errorf("scanning the composed query: %w", err)
	}
	if rows.Next() {
		return zero, ErrMultipleRows
	}
	if err := rows.Err(); err != nil {
		return zero, fmt.Errorf("reading the composed query: %w", err)
	}
	return out, nil
}

// Rows runs the query and yields results one at a time, buffering nothing.
func (q *ComposedQuery[R]) Rows(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R

		sel, err := q.build()
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := q.exec(ctx, sel)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()

		scan := q.newScan()
		for rows.Next() {
			r, err := scan(rows)
			if err != nil {
				yield(zero, fmt.Errorf("scanning the composed query: %w", err))
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("reading the composed query: %w", err))
		}
	}
}

// Count reports how many rows the query would return, respecting Limit, Offset
// and Distinct.
func (q *ComposedQuery[R]) Count(ctx context.Context) (int64, error) {
	sel, err := q.build()
	if err != nil {
		return 0, err
	}
	sql, args, err := expr.CountFrom(sel).Compile()
	if err != nil {
		return 0, err
	}
	if q.ex == nil {
		return 0, errors.New("the composed query has no executor")
	}
	rows, err := q.ex.Query(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("counting the composed query: %w", err)
	}
	defer rows.Close()

	var n int64
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("counting the composed query: %w", err)
		}
		return 0, errors.New("counting the composed query: the server returned no row")
	}
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("counting the composed query: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("counting the composed query: %w", err)
	}
	return n, nil
}

// term makes an entity query usable inside another statement.
func (q *Query[E]) term() (*expr.Select, error) { return q.build() }

// term makes a projection query usable inside another statement.
func (q *SelectQuery[E, R]) term() (*expr.Select, error) { return q.build() }

// sourceTerm makes a projection query a row source.
func (q *SelectQuery[E, R]) sourceTerm() (expr.Subquery, []string, error) {
	sel, err := q.build()
	if err != nil {
		return nil, nil, err
	}
	return sel, q.outputs(), nil
}

// valueTerm makes a projection query usable where a value is expected.
func (q *SelectQuery[E, R]) valueTerm() (expr.Subquery, int, error) {
	sel, err := q.build()
	if err != nil {
		return nil, 0, err
	}
	return sel, len(q.proj.items), nil
}

// outputs reports the projection's result names, so that a projection query can
// be a derived table or a CTE when every expression it selects was aliased.
func (q *SelectQuery[E, R]) outputs() []string {
	out := make([]string, 0, len(q.proj.items))
	for _, it := range q.proj.items {
		out = append(out, it.Alias)
	}
	return out
}

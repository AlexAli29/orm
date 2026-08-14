package orm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// The projection query.
//
// An entity query answers "which rows of this table", and its result shape is
// the entity. A projection query answers the same question and returns
// something else: the expressions asked for, in the Go shape the projection
// binds them to. Everything about choosing rows is shared — the same
// predicates, the same ordering, the same scope rules, the same compiler — and
// only the select list and the scanner differ.
//
// Relations are deliberately absent. A relation is loaded by matching a key of
// the root rows against the target table, and a projection need not select any
// key at all; attaching relations to an arbitrary result shape would mean
// either silently selecting columns nobody asked for or failing at run time on
// shapes that happen to lack them. Entity queries load relations; projections
// return values.

// Select starts a projection query over an entity's table.
//
//	orm.Select(db.Users, UserSummaries).
//	    Where(Users.Active.Eq(true)).
//	    OrderBy(Users.CreatedAt.Desc()).
//	    Limit(50).
//	    All(ctx)
//
// It is a function rather than a method because the result type is a second
// type parameter, and a Go method cannot introduce one.
func Select[E, R any](r *Repo[E], p Projection[E, R]) *SelectQuery[E, R] {
	q := &SelectQuery[E, R]{proj: p}
	if r == nil {
		q.fail(errors.New("Select was given no repository"))
		return q
	}
	q.repo = r
	q.source = r.source
	if err := r.meta.validate(); err != nil {
		q.fail(err)
	}
	if err := p.validate(); err != nil {
		q.fail(err)
	}
	return q
}

// SelectFrom starts a projection query reading from a particular occurrence of
// the entity's table, which is how a projection selects through an alias.
func SelectFrom[E, R any](r *Repo[E], src *Source, p Projection[E, R]) *SelectQuery[E, R] {
	q := Select(r, p)
	switch {
	case src == nil:
		q.fail(errors.New("SelectFrom was given no source"))
	case q.repo != nil && q.repo.meta != nil && !sourceIsTable(src, q.repo.meta.Table):
		q.fail(fmt.Errorf("SelectFrom: %s is not an occurrence of %s", src, q.repo.meta.Table))
	default:
		q.source = src
	}
	return q
}

// SelectQuery builds a SELECT of a projection over entity E.
//
// Its methods mutate the builder and return it, and mistakes are recorded
// rather than raised — the same contract [Query] has, for the same reason: a
// caller who made two of them is better served seeing both. When any has been
// recorded, no terminal operation touches PostgreSQL.
//
// A SelectQuery is mutable and is not safe for concurrent use. [SelectQuery.Clone]
// is what makes one builder the base for several.
type SelectQuery[E, R any] struct {
	repo       *Repo[E]
	source     *Source
	proj       Projection[E, R]
	wheres     []Predicate[E]
	orderBy    []Order[E]
	groupBy    []expr.Node
	distinctOn []expr.Node
	havings    []Predicate[E]
	limit      *int
	offset     *int
	distinct   bool
	forUpdate  bool
	lock       expr.Lock
	errs       []error
}

func (q *SelectQuery[E, R]) fail(err error) {
	if err != nil {
		q.errs = append(q.errs, err)
	}
}

// Clone returns an independent copy. Every slice the builder owns is copied,
// and the numbers behind limit and offset are copied rather than shared.
func (q *SelectQuery[E, R]) Clone() *SelectQuery[E, R] {
	out := &SelectQuery[E, R]{
		repo:       q.repo,
		source:     q.source,
		proj:       q.proj,
		wheres:     slices.Clone(q.wheres),
		orderBy:    slices.Clone(q.orderBy),
		groupBy:    slices.Clone(q.groupBy),
		distinctOn: slices.Clone(q.distinctOn),
		havings:    slices.Clone(q.havings),
		distinct:   q.distinct,
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

// Where adds conditions, joined with AND. Calling it again adds more.
func (q *SelectQuery[E, R]) Where(ps ...Predicate[E]) *SelectQuery[E, R] {
	for _, p := range ps {
		// An aggregate belongs in HAVING. PostgreSQL says so too, but it says
		// it after a round trip and in terms of the statement it received.
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

// GroupBy groups the rows the query returns.
//
//	orm.Select(db.Posts, postsPerAuthor).GroupBy(Posts.AuthorID)
//
// The expressions are grouped in the order given and are never sorted: the
// order decides the grouping PostgreSQL performs. Each has to be in the
// query's scope, which the compiler checks like any other reference.
//
// PostgreSQL decides whether the select list is consistent with the grouping —
// a bare column that is neither grouped nor aggregated is its error to report,
// and it reports it precisely. Reimplementing that judgement here would mean an
// approximation of a functional-dependency analysis, which would be wrong in
// exactly the cases worth getting right.
func (q *SelectQuery[E, R]) GroupBy(gs ...Grouping[E]) *SelectQuery[E, R] {
	for _, g := range gs {
		node := g.groupNode()
		if containsAggregate(node) {
			q.fail(errors.New("GroupBy cannot contain an aggregate; a group is defined by the values it groups on"))
			continue
		}
		q.groupBy = append(q.groupBy, node)
	}
	return q
}

// Having filters the groups, after grouping.
//
//	.GroupBy(Posts.AuthorID).Having(orm.Count[Post]().Gt(5))
//
// Conditions are joined with AND, and calling it again adds more. This is where
// an aggregate condition belongs; [SelectQuery.Where] refuses one, because WHERE
// runs before the groups exist.
func (q *SelectQuery[E, R]) Having(ps ...Predicate[E]) *SelectQuery[E, R] {
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
func (q *SelectQuery[E, R]) OrderBy(os ...Order[E]) *SelectQuery[E, R] {
	q.orderBy = append(q.orderBy, os...)
	return q
}

// Limit caps the rows returned. A limit of zero returns nothing.
func (q *SelectQuery[E, R]) Limit(n int) *SelectQuery[E, R] {
	if n < 0 {
		q.fail(fmt.Errorf("negative limit %d", n))
		return q
	}
	q.limit = &n
	return q
}

// Offset skips rows before returning any.
func (q *SelectQuery[E, R]) Offset(n int) *SelectQuery[E, R] {
	if n < 0 {
		q.fail(fmt.Errorf("negative offset %d", n))
		return q
	}
	q.offset = &n
	return q
}

// Distinct removes duplicate rows from the result.
//
// It compares the whole select list, which is what SQL's DISTINCT means. It is
// not [SelectQuery.DistinctOn], which chooses a representative row per key and
// needs an ordering to say which — a different feature with a different
// contract.
func (q *SelectQuery[E, R]) Distinct() *SelectQuery[E, R] {
	q.distinct = true
	return q
}

// DistinctOn keeps the first row of each group of equal expressions.
//
//	orm.Select(db.Posts, latest).
//	    DistinctOn(Posts.AuthorID).
//	    OrderBy(Posts.AuthorID.Asc(), Posts.CreatedAt.Desc())
//
// Which row is kept depends on the ORDER BY, whose leading terms PostgreSQL
// requires to match these expressions. That requirement is the server's to
// enforce: deciding whether two expressions are the same one is a judgement
// about SQL equivalence this package cannot make soundly, and an over-strict
// version would refuse queries PostgreSQL runs happily.
func (q *SelectQuery[E, R]) DistinctOn(gs ...Grouping[E]) *SelectQuery[E, R] {
	for _, g := range gs {
		node := g.groupNode()
		if containsAggregate(node) {
			q.fail(errors.New("DistinctOn cannot contain an aggregate; it chooses among rows, which happens before any group exists"))
			continue
		}
		q.distinctOn = append(q.distinctOn, node)
	}
	return q
}

// ForUpdate locks the rows the statement returns.
func (q *SelectQuery[E, R]) ForUpdate() *SelectQuery[E, R] {
	q.forUpdate = true
	return q
}

// SQL renders the statement and its parameters without running it.
func (q *SelectQuery[E, R]) SQL() (string, []any, error) {
	sel, err := q.build()
	if err != nil {
		return "", nil, err
	}
	return sel.Compile()
}

// build assembles the statement, refusing before any of it if the builder
// recorded a mistake.
func (q *SelectQuery[E, R]) build() (*expr.Select, error) {
	if len(q.errs) > 0 {
		return nil, errors.Join(q.errs...)
	}
	sel := &expr.Select{
		From:       q.source,
		Items:      slices.Clone(q.proj.items),
		Distinct:   q.distinct,
		DistinctOn: slices.Clone(q.distinctOn),
		GroupBy:    slices.Clone(q.groupBy),
		Limit:      q.limit,
		Offset:     q.offset,
		ForUpdate:  q.forUpdate,
		Lock:       q.lock,
	}
	if where := And(q.wheres...); !expr.IsTrue(where.node) {
		sel.Where = where.node
	}
	if having := And(q.havings...); !expr.IsTrue(having.node) {
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
func (q *SelectQuery[E, R]) exec(ctx context.Context, stmt expr.Statement) (pgxRows, error) {
	sql, args, err := stmt.Compile()
	if err != nil {
		return nil, err
	}
	if q.repo == nil || q.repo.ex == nil {
		return nil, fmt.Errorf("selecting from %s: the repository has no executor", q.table())
	}
	rows, err := q.repo.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("selecting from %s: %w", q.table(), err)
	}
	return rows, nil
}

func (q *SelectQuery[E, R]) table() TableID {
	if q.repo != nil && q.repo.meta != nil {
		return q.repo.meta.Table
	}
	return TableID{}
}

// All runs the query and returns every row.
func (q *SelectQuery[E, R]) All(ctx context.Context) ([]R, error) {
	sel, err := q.build()
	if err != nil {
		return nil, err
	}
	rows, err := q.exec(ctx, sel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scan := q.proj.newScan()
	var out []R
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning %s: %w", q.table(), err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", q.table(), err)
	}
	return out, nil
}

// One runs the query and returns the single row it selects.
//
// No rows is [ErrNotFound] and more than one is [ErrMultipleRows]. The
// statement is limited to two rows rather than to one, so the second can be
// observed without reading the rest — and the builder is not changed, so a
// query is not quietly narrowed by having been asked for one row.
func (q *SelectQuery[E, R]) One(ctx context.Context) (R, error) {
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
			return zero, fmt.Errorf("reading %s: %w", q.table(), err)
		}
		return zero, ErrNotFound
	}
	out, err := q.proj.newScan()(rows)
	if err != nil {
		return zero, fmt.Errorf("scanning %s: %w", q.table(), err)
	}
	if rows.Next() {
		return zero, ErrMultipleRows
	}
	if err := rows.Err(); err != nil {
		return zero, fmt.Errorf("reading %s: %w", q.table(), err)
	}
	return out, nil
}

// Count reports how many rows the query would return.
//
// It counts the rowset the projection selects, which is not the same question
// as [Count] the aggregate: this respects Limit, Offset and Distinct, and a
// grouped query counts its groups.
func (q *SelectQuery[E, R]) Count(ctx context.Context) (int64, error) {
	sel, err := q.build()
	if err != nil {
		return 0, err
	}
	rows, err := q.exec(ctx, expr.CountFrom(sel))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n int64
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("counting %s: %w", q.table(), err)
		}
		return 0, fmt.Errorf("counting %s: the server returned no row", q.table())
	}
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", q.table(), err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("counting %s: %w", q.table(), err)
	}
	return n, nil
}

// Exists reports whether the query would return any row.
func (q *SelectQuery[E, R]) Exists(ctx context.Context) (bool, error) {
	sel, err := q.build()
	if err != nil {
		return false, err
	}
	rows, err := q.exec(ctx, expr.ExistsFrom(sel))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var found bool
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("testing %s: %w", q.table(), err)
		}
		return false, fmt.Errorf("testing %s: the server returned no row", q.table())
	}
	if err := rows.Scan(&found); err != nil {
		return false, fmt.Errorf("testing %s: %w", q.table(), err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("testing %s: %w", q.table(), err)
	}
	return found, nil
}

// Rows runs the query and yields results one at a time:
//
//	for summary, err := range orm.Select(db.Users, UserSummaries).Rows(ctx) {
//	    if err != nil {
//	        return err
//	    }
//	    process(summary)
//	}
//
// Nothing is buffered. Stopping early closes the rows and releases the
// connection, so a break is safe. A projection has no relations to load, so
// unlike the entity form there is no shape it has to refuse.
func (q *SelectQuery[E, R]) Rows(ctx context.Context) iter.Seq2[R, error] {
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
		// This runs whether the loop finishes, returns, or breaks, which is
		// what keeps an early stop from holding the connection.
		defer rows.Close()

		scan := q.proj.newScan()
		for rows.Next() {
			r, err := scan(rows)
			if err != nil {
				yield(zero, fmt.Errorf("scanning %s: %w", q.table(), err))
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("reading %s: %w", q.table(), err))
		}
	}
}

package orm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"

	"github.com/AlexAli29/orm/internal/expr"
)

// UNION ALL.
//
// # Why a free function
//
// A set operation has no owner. Writing a.UnionAll(b) would say the left branch
// performs the operation and the right one is an argument, which is not what
// UNION ALL means and not how PostgreSQL treats it: the branches are peers, and
// A UNION ALL B UNION ALL C is one operation over three inputs rather than a
// tree of pairs somebody associated. A method would also have to be written on
// every builder that can be a branch, which is three copies of one rule.
//
// # Why a separate result type
//
// The result is not a [ComposedQuery]. That type is a SELECT over sources and it
// satisfies [Term], whose contract is to render a *expr.Select — and two of
// Term's callers genuinely need one: EXISTS canonicalises the statement it is
// given, and a recursive CTE compares the anchor's select list against the
// recursive term's. A compound is not a Select, so making ComposedQuery able to
// hold one would leave a value that satisfies Term and fails it at run time.
//
// A UnionQuery is therefore its own type. It can be read, ordered by its own
// output columns, limited, used as a branch of another union, used as a derived
// table or an ordinary CTE body through [SourceTerm], and used in a membership
// test or as a scalar value through [ValueTerm]. Each of those is a separate
// capability with its own requirements — a source declares column names, a value
// subquery declares an arity — and none of them is Term.
//
// A compound is still not the body of a recursive CTE, or the statement inside
// an EXISTS: both compare or rewrite select internals, and so genuinely need a
// Select.

// Branch is one input of a set operation.
//
// It is exported so that it can be written in a signature, and closed so that
// only this package can implement it: the method is unexported, and a branch
// implemented outside would be one that could arrive without a result shape —
// which is the whole thing the operation validates.
//
// Closed means cannot implement, not cannot name. Embedding the interface in a
// struct satisfies it with nothing behind it, and calling the method then panics
// — Go's rule for every embedded nil, true of every interface in this package
// and every other. It is not a route to reaching the operation with a value it
// would accept.
//
// It is implemented by [Query], [SelectQuery] and [UnionQuery]. A ComposedQuery
// implements it too, so a composed statement can be a branch.
type Branch[R any] interface {
	unionBranch() (branchTerm[R], error)
}

// branchTerm is what a branch contributes: the tree to render, the result shape
// to validate, and the scanner that reads it.
type branchTerm[R any] struct {
	sub     expr.Subquery
	shape   resultShape
	newScan func() func(pgxRows) (R, error)
	// ex is the executor the branch was built with, used only to give the
	// union one when the caller did not set it explicitly.
	ex Executor
}

// UnionAll concatenates the rows of two or more branches, keeping duplicates.
//
//	orm.UnionAll(
//	    orm.Rows(active).From(Users.Source()).Where(...),
//	    orm.Rows(active).From(Archived.Source()).Where(...),
//	).All(ctx)
//
// Every branch produces the same Go result type R, which the compiler enforces,
// and the same result shape, which is checked here: the number of columns, and
// each column's Go type and nullability, positionally. A mismatch is a builder
// error and no SQL is produced.
//
// Duplicates are kept. That is what ALL means, and it is also the cheaper
// operation — removing them costs a sort or a hash of the whole result.
func UnionAll[R any](branches ...Branch[R]) *UnionQuery[R] {
	q := &UnionQuery[R]{op: expr.SetUnionAll}
	if len(branches) < 2 {
		q.fail(fmt.Errorf("UNION ALL needs at least two branches and was given %d", len(branches)))
		return q
	}

	var first resultShape
	for i, b := range branches {
		n := i + 1
		if b == nil {
			q.fail(fmt.Errorf("UNION ALL branch %d is nil", n))
			continue
		}
		term, err := b.unionBranch()
		if err != nil {
			q.fail(fmt.Errorf("UNION ALL branch %d: %w", n, err))
			continue
		}
		if term.sub == nil {
			q.fail(fmt.Errorf("UNION ALL branch %d produced no statement", n))
			continue
		}
		// Not everything a query can be is something a branch can be. A locking
		// clause is the one PostgreSQL refuses outright, and it is refused here
		// rather than when the statement is written, so the caller is told where
		// the branch was handed over.
		if err := expr.ValidateBranch(term.sub); err != nil {
			q.fail(fmt.Errorf("UNION ALL branch %d: %w", n, err))
			continue
		}
		if i == 0 {
			// The first branch owns the result: its scanner reads every row,
			// and PostgreSQL takes the compound's output names from it. That is
			// only safe once every later branch is proven to produce the same
			// shape, which is what the comparison below does — and it compares
			// against this shape rather than against its neighbour, so branch 3
			// cannot drift from branch 1 by agreeing with branch 2.
			first = term.shape
			q.shape = term.shape
			q.newScan = term.newScan
			q.ex = term.ex
		} else if err := compareResultShapes(first, term.shape, 1, n); err != nil {
			q.fail(err)
			continue
		}
		q.branches = append(q.branches, term.sub)
	}
	if q.newScan == nil && len(q.errs) == 0 {
		q.fail(errors.New("UNION ALL branch 1 has no scanner, so the result could not be read"))
	}
	return q
}

// UnionQuery is the statement a set operation produces.
//
// Its methods mutate the builder and return it, and mistakes are recorded rather
// than raised — the contract [Query], [SelectQuery] and [ComposedQuery] have. It
// is mutable and not safe for concurrent use.
type UnionQuery[R any] struct {
	ex       Executor
	op       expr.SetKind
	branches []expr.Subquery
	// shape and newScan are branch 1's, kept so that this query can itself be a
	// branch without the shape being rebuilt from anything — least of all from
	// rendered SQL.
	shape   resultShape
	newScan func() func(pgxRows) (R, error)

	with    []*Source
	orderBy []expr.OutputOrder
	limit   *int
	offset  *int
	errs    []error
}

func (q *UnionQuery[R]) fail(err error) {
	if err != nil {
		q.errs = append(q.errs, err)
	}
}

// With declares named queries for the whole operation, in the order given.
//
//	recent := orm.CTE("recent", ...)
//	orm.UnionAll(fromPosts, fromDrafts).With(recent)
//
// PostgreSQL puts a compound's WITH clause in front of the operation, where it
// is evaluated once and is visible to every branch. That is what makes this the
// way to share a named query between branches: a branch's own WITH belongs to
// that branch and is parenthesised to keep it there, so one branch cannot read
// what another declared.
//
// The items are written before the branches, so their parameters are numbered
// first — one statement, one parameter list, in the order the SQL reads.
//
// A later item may name an earlier one; an earlier one may not name a later one.
// Both are checked when the statement is built, against the item rather than
// against a relation PostgreSQL could not resolve.
func (q *UnionQuery[R]) With(ctes ...*Source) *UnionQuery[R] {
	for i, c := range ctes {
		if err := validWithItem(i, c); err != nil {
			q.fail(err)
			continue
		}
		q.with = append(q.with, c)
	}
	return q
}

// Using sets the executor the statement runs on.
func (q *UnionQuery[R]) Using(ex Executor) *UnionQuery[R] {
	q.ex = ex
	return q
}

// OrderBy sorts the whole result by the operation's own output columns.
//
//	thingID := orm.Named("thing_id", orm.Of(Users.ID))
//	orm.UnionAll(fromUsers, fromArchive).OrderBy(thingID.Desc()).Limit(10)
//
// PostgreSQL attaches a compound's ORDER BY to the operation rather than to its
// last branch, which is why this is here and not pushed down: ordering one
// branch orders that branch, and the concatenation is then in whatever order the
// branches produced.
//
// # Why the terms are names
//
// A compound's ORDER BY takes an output column name, or an ordinal, and nothing
// else. A qualified reference is refused —
//
//	ERROR: missing FROM-clause entry for table "t"
//
// and so is any expression, even one built over an output name:
//
//	ERROR: invalid UNION/INTERSECT/EXCEPT ORDER BY clause
//
// So the term is an [OutputOrder] rather than an [Order]: a name and a
// direction, with nowhere to put the expression PostgreSQL would reject.
//
// # Which names
//
// The compound's output names are the first branch's, which is PostgreSQL's rule
// and the same one that decides what a union provides when it is a source. A
// name no branch declared is refused here, naming the ones there are — an
// ordering term is checked against the result shape, not against a guess.
//
// The names have to be declared. This package does not model the names
// PostgreSQL derives for itself from a bare column, because deriving them would
// mean claiming to know how the server names every expression; so a branch whose
// columns are not aliased has nothing to order by, and says so.
func (q *UnionQuery[R]) OrderBy(os ...OutputOrder) *UnionQuery[R] {
	for i, o := range os {
		switch {
		case o.name == "":
			q.fail(fmt.Errorf("ordering term %d names no output column", i+1))
			continue
		case !q.shape.known():
			// The union already failed to validate; the mistake that produced
			// that is the one worth reporting, and this term is not it.
		case !q.provides(o.name):
			q.fail(fmt.Errorf("this set operation has no result column %q, so it cannot be ordered by one; %s",
				o.name, q.describeOutputs()))
			continue
		}
		q.orderBy = append(q.orderBy, expr.OutputOrder{Name: o.name, Desc: o.desc})
	}
	return q
}

// provides reports whether the result has a column of this name.
//
// A yes is enough. A name carried twice would make the term ambiguous, and
// PostgreSQL says so — but a shape is refused at construction unless its names
// identify its columns, so there is no such shape to receive here. Asking again
// would be this consumer re-litigating a fact the shape already settled, which
// is how the check came to be missing from one of four consumers in the first
// place.
func (q *UnionQuery[R]) provides(name string) bool {
	for _, slot := range q.shape.slots {
		if slot.alias == name {
			return true
		}
	}
	return false
}

// describeOutputs says what the result does provide, so that a rejected term
// comes with the list rather than with an invitation to guess again.
func (q *UnionQuery[R]) describeOutputs() string {
	named := make([]string, 0, q.shape.columns())
	for _, slot := range q.shape.slots {
		if slot.alias != "" {
			named = append(named, strconv.Quote(slot.alias))
		}
	}
	if len(named) == 0 {
		return "its first branch names none of its columns, and a compound takes its output names from that branch; declare them with As"
	}
	return "it provides " + strings.Join(named, ", ")
}

// Limit caps the number of rows of the whole result.
//
// It applies to the concatenation, not to a branch. Which rows survive is
// decided by the ordering, and without one PostgreSQL may produce a compound's
// rows in any order it likes — so a limit with no [UnionQuery.OrderBy] returns
// some rows rather than the first ones.
func (q *UnionQuery[R]) Limit(n int) *UnionQuery[R] {
	if n < 0 {
		q.fail(fmt.Errorf("LIMIT %d is negative", n))
		return q
	}
	q.limit = &n
	return q
}

// Offset skips rows of the whole result.
func (q *UnionQuery[R]) Offset(n int) *UnionQuery[R] {
	if n < 0 {
		q.fail(fmt.Errorf("OFFSET %d is negative", n))
		return q
	}
	q.offset = &n
	return q
}

// build renders the compound, or reports every mistake made building it.
func (q *UnionQuery[R]) build() (*expr.Compound, error) {
	if len(q.errs) > 0 {
		return nil, errors.Join(q.errs...)
	}
	if len(q.branches) < 2 {
		return nil, fmt.Errorf("UNION ALL needs at least two branches and has %d", len(q.branches))
	}
	c := &expr.Compound{
		Op:       q.op,
		With:     slices.Clone(q.with),
		Branches: q.branches,
		OrderBy:  slices.Clone(q.orderBy),
		Limit:    q.limit,
		Offset:   q.offset,
	}
	return c, nil
}

// SQL renders the statement and the arguments it binds.
//
// Both branches are compiled by one call, so the placeholders are numbered
// across the whole statement rather than restarted per branch.
func (q *UnionQuery[R]) SQL() (string, []any, error) {
	c, err := q.build()
	if err != nil {
		return "", nil, err
	}
	return c.Compile()
}

// unionBranch makes a set operation usable as a branch of another one.
//
// The shape travels with the value: it is the one branch 1 supplied and every
// other branch was checked against, never something recomputed from SQL.
func (q *UnionQuery[R]) unionBranch() (branchTerm[R], error) {
	c, err := q.build()
	if err != nil {
		return branchTerm[R]{}, err
	}
	return branchTerm[R]{sub: c, shape: q.shape, newScan: q.newScan, ex: q.ex}, nil
}

// sourceTerm makes a set operation a row source: a derived table, or the body of
// an ordinary CTE.
//
//	recent := orm.Sub("recent", orm.UnionAll(fromPosts, fromArchive))
//	orm.Compose(ex, shape).From(recent)
//
// The column names are the first branch's, taken from the result shape the
// union was validated with. That is PostgreSQL's own rule — a compound's output
// columns are named after its first branch — so the names a caller addresses the
// source by are the names the server will use, rather than a second opinion
// about them. They are read from the shape and not from the rendered statement:
// the shape is what the branches were checked against, and it is still here.
//
// A union whose first branch does not name its columns cannot be a source. That
// is the same rule an ordinary derived table has, and the diagnostic says which
// branch has to be fixed, because naming the second one would not help.
func (q *UnionQuery[R]) sourceTerm() (expr.Subquery, []string, error) {
	c, err := q.build()
	if err != nil {
		return nil, nil, err
	}
	outs := make([]string, 0, q.shape.columns())
	for i, slot := range q.shape.slots {
		if slot.alias == "" {
			return nil, nil, fmt.Errorf("column %d of the set operation has no name; a compound takes its output names from its first branch, so declare that branch's columns with Named", i+1)
		}
		outs = append(outs, slot.alias)
	}
	return c, outs, nil
}

// valueTerm makes a set operation usable where a value is expected.
//
//	orm.InSub(Users.ID, orm.UnionAll(authorsOfPosts, authorsOfDrafts))
//	orm.Scalar[User, int64](orm.UnionAll(oneCount, otherCount))
//
// The arity is the result shape's, which is the shape branch 1 supplied and
// every other branch was checked against. A compound has no select list of its
// own to count, and counting one branch's would be taking that branch's word for
// what the whole operation returns — which is exactly the question the shape
// already answered.
//
// Whether the values are comparable to the left-hand side of an IN, or readable
// as the Go type a Scalar was asked for, is not decided here. The first is
// PostgreSQL's and it decides it precisely; the second is the caller's, as it is
// for every other statement these two take.
func (q *UnionQuery[R]) valueTerm() (expr.Subquery, int, error) {
	c, err := q.build()
	if err != nil {
		return nil, 0, err
	}
	if !q.shape.known() {
		return nil, 0, errors.New("this set operation has no result shape, so how many columns it returns is unknown")
	}
	return c, q.shape.columns(), nil
}

func (q *UnionQuery[R]) exec(ctx context.Context, c *expr.Compound) (pgxRows, error) {
	if q.newScan == nil {
		return nil, errors.New("this set operation has no result shape, so it cannot be read")
	}
	sql, args, err := c.Compile()
	if err != nil {
		return nil, err
	}
	if q.ex == nil {
		return nil, errors.New("the set operation has no executor")
	}
	rows, err := q.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("running the set operation: %w", err)
	}
	return rows, nil
}

// All runs the statement and returns every row, in branch order.
func (q *UnionQuery[R]) All(ctx context.Context) ([]R, error) {
	c, err := q.build()
	if err != nil {
		return nil, err
	}
	rows, err := q.exec(ctx, c)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scan := q.newScan()
	var out []R
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning the set operation: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the set operation: %w", err)
	}
	return out, nil
}

// One runs the statement and returns the single row it produces, reporting
// [ErrNotFound] when there was none and [ErrMultipleRows] when there was more
// than one.
func (q *UnionQuery[R]) One(ctx context.Context) (R, error) {
	var zero R
	c, err := q.build()
	if err != nil {
		return zero, err
	}
	if c.Limit == nil || *c.Limit > 2 {
		two := 2
		c.Limit = &two
	}
	rows, err := q.exec(ctx, c)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	scan := q.newScan()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, fmt.Errorf("reading the set operation: %w", err)
		}
		return zero, ErrNotFound
	}
	out, err := scan(rows)
	if err != nil {
		return zero, fmt.Errorf("scanning the set operation: %w", err)
	}
	if rows.Next() {
		return zero, ErrMultipleRows
	}
	if err := rows.Err(); err != nil {
		return zero, fmt.Errorf("reading the set operation: %w", err)
	}
	return out, nil
}

// Rows runs the statement and yields each row as it arrives.
func (q *UnionQuery[R]) Rows(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		c, err := q.build()
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := q.exec(ctx, c)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()

		scan := q.newScan()
		for rows.Next() {
			r, err := scan(rows)
			if err != nil {
				yield(zero, fmt.Errorf("scanning the set operation: %w", err))
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("reading the set operation: %w", err))
		}
	}
}

// The builders that can be a branch.
//
// Each hands over three things and nothing else: the tree, the result shape it
// already knows, and the scanner that reads that shape. None of them describes
// the result twice — the projection branches use the shape their Projection was
// built with, and the entity branch uses the generated descriptor.

// unionBranch makes an entity query a branch.
//
// The shape comes from the generated entity metadata rather than from E: the
// column order is the descriptor's, the nullability is the catalog's NotNull,
// and the Go types are the destinations the generated Dest hands out. A shape
// built from reflect.TypeOf(E) would be a second description of a schema the
// generated code already states, and the two could disagree.
func (q *Query[E]) unionBranch() (branchTerm[E], error) {
	sel, err := q.build()
	if err != nil {
		return branchTerm[E]{}, err
	}
	if q.repo == nil {
		return branchTerm[E]{}, errors.New("the entity query has no repository")
	}
	shape, err := entityShape(q.repo.meta)
	if err != nil {
		return branchTerm[E]{}, err
	}
	// The scanner is the entity scanner an ordinary query uses. A set operation
	// reads rows of the same shape, so it reads them the same way.
	return branchTerm[E]{sub: sel, shape: shape, newScan: q.scanner, ex: q.repo.ex}, nil
}

// unionBranch makes a projection query a branch.
func (q *SelectQuery[E, R]) unionBranch() (branchTerm[R], error) {
	sel, err := q.build()
	if err != nil {
		return branchTerm[R]{}, err
	}
	if err := q.proj.validate(); err != nil {
		return branchTerm[R]{}, err
	}
	var ex Executor
	if q.repo != nil {
		ex = q.repo.ex
	}
	return branchTerm[R]{sub: sel, shape: q.proj.shape, newScan: q.proj.newScan, ex: ex}, nil
}

// unionBranch makes a composed query a branch.
func (q *ComposedQuery[R]) unionBranch() (branchTerm[R], error) {
	sel, err := q.build()
	if err != nil {
		return branchTerm[R]{}, err
	}
	if !q.shape.known() {
		// A query built with Rows selects declarations and has no typed result:
		// it exists to be a source. Letting one be a branch would mean a union
		// with nothing to scan into.
		return branchTerm[R]{}, errors.New("this query was built to be a source and has no result shape; build it with Compose to use it as a branch")
	}
	return branchTerm[R]{sub: sel, shape: q.shape, newScan: q.newScan, ex: q.ex}, nil
}

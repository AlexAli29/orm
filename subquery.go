package orm

import (
	"errors"
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// Subqueries.
//
// A statement stops being the end of the line here: it becomes a FROM item, a
// value, or a test. All three nest the same way — the inner tree is compiled by
// the enclosing statement's writer — so all three share one parameter
// numbering, one scope stack and one set of identifier rules. Nothing in this
// file renders SQL to a string and pastes it somewhere.

// SourceTerm is a read query that can be a row source.
//
// It is what [Sub] and [CTE] take. The two things a source needs are a
// statement to nest and the names of the columns it provides, and this returns
// both together — a column of a derived table or of a CTE is addressed by name,
// so a query that cannot say what its columns are called cannot be one.
//
// It is closed by an unexported method — meaning nothing outside this package
// can implement it, not that nothing can name it: embedding it in a struct
// satisfies it with nothing behind it, and calling the method then panics, as it
// does for every embedded nil in Go. It is a different interface from [Term]
// rather than a widening of it. Term's contract is to produce an
// *expr.Select, which EXISTS and a recursive CTE genuinely need because they
// rewrite and compare select internals. A set operation is not a Select, so a
// Term that could hold one would be a value that satisfies Term and fails it
// when something reads it. Splitting the two makes "can be a source" and "is a
// SELECT" separate claims, which is what they are.
//
// It is satisfied by [ComposedQuery], [SelectQuery] and [UnionQuery]. An entity
// query is deliberately not one: it selects its descriptor's columns and
// declares no output names, so a derived table over it would have nothing for
// its columns to be addressed by. That used to compile and fail when the
// statement was built; now it does not compile.
type SourceTerm interface {
	// sourceTerm renders the statement to nest and reports the names of the
	// columns it provides, in select-list order.
	sourceTerm() (expr.Subquery, []string, error)
}

// Sub builds a derived table: a query used as a source.
//
// The alias is required, because a derived table with no name has nothing for
// its columns to qualify against, and its output columns are the ones the query
// declared — so a column of it is read with [Ref], typed by the declaration
// rather than by a string:
//
//	userID := orm.Named("user_id", orm.Of(Posts.AuthorID))
//	count  := orm.Named("post_count", orm.Count[orm.Composed]())
//
//	stats := orm.Sub("post_stats", orm.Rows(userID, count).
//	    From(Posts.Source()).
//	    GroupBy(orm.Of(Posts.AuthorID)))
//
//	orm.Ref(stats, count)   // Value[Composed, int64]
//
// The query is not consumed. Two derived tables built from one query are two
// independent sources, and nothing about compiling either is stored in it.
func Sub(alias string, t SourceTerm) *Source {
	sub, outs, err := readSource(t)
	if err != nil {
		return expr.FailedDerived(alias, err)
	}
	return expr.NewDerived(alias, sub, outs)
}

// readSource renders a source term and reports the output names it provides.
//
// The statement is an expr.Subquery rather than an *expr.Select, which is what
// lets a set operation be a source: a derived table and a WITH item hold a
// Subquery already, so neither of them learns what a compound is. The writer
// parenthesises both, so the compound is delimited by the grammar it is written
// into rather than by anything decided here.
func readSource(t SourceTerm) (expr.Subquery, []string, error) {
	if t == nil {
		return nil, nil, errors.New("a row source was given no query")
	}
	sub, outs, err := t.sourceTerm()
	if err != nil {
		return nil, nil, err
	}
	if sub == nil {
		return nil, nil, errors.New("a row source was given a query that produced no statement")
	}
	if err := namedOutputs(outs); err != nil {
		return nil, nil, err
	}
	return sub, outs, nil
}

// source renders a term that has to be a plain SELECT, and reports the output
// names it provides.
//
// It is what the recursive CTE builder uses. Recursion compares the anchor's
// select list against the recursive term's, which is a question only a Select
// can answer, so this path keeps the concrete requirement and the runtime check
// that comes with it.
func source(t Term) (*expr.Select, []string, error) {
	if t == nil {
		return nil, nil, errors.New("a row source was given no query")
	}
	sel, err := t.term()
	if err != nil {
		return nil, nil, err
	}
	named, ok := t.(interface{ outputs() []string })
	if !ok {
		return nil, nil, errors.New("a row source has to be built from Rows or Compose, whose select list declares its column names")
	}
	outs := named.outputs()
	if err := namedOutputs(outs); err != nil {
		return nil, nil, err
	}
	return sel, outs, nil
}

// namedOutputs refuses a select list that does not name every column.
//
// One rule, spelled once, because a derived table and a CTE address their
// columns the same way and a second copy of it would be a second answer.
func namedOutputs(outs []string) error {
	if len(outs) == 0 {
		return errors.New("a row source declares no columns")
	}
	for i, name := range outs {
		if name == "" {
			return fmt.Errorf("output column %d of the row source has no name; every column of a derived table or a CTE is declared with Named", i+1)
		}
	}
	return nil
}

// ValueTerm is a read query that can be used where a value is expected.
//
// It is what [InSub], [NotInSub] and [Scalar] take. A value subquery needs one
// thing from the query beyond the statement itself: how many columns the result
// has. IN compares as many columns as the expression on its left, and a scalar
// subquery reads exactly one — so the number is what decides whether the SQL
// is valid, and it is known when the query is built.
//
// It is not [SourceTerm], and the two are kept apart because they ask for
// different things. A source's columns are addressed by name, so a source term
// has to declare them; a value subquery's columns are not addressed at all, so
// requiring names would refuse queries PostgreSQL is perfectly happy with:
//
//	orm.Scalar[User, int64](orm.Rows(orm.Named("n", orm.Count[orm.Composed]())).From(...))
//	orm.InSub(Users.ID, orm.Compose(nil, orm.Project1(orm.Of(Posts.AuthorID), ...)).From(...))
//
// The second of those selects an unnamed column and is a valid membership test.
// A query can be a value subquery without being a source, and the other way
// round, so neither interface is defined in terms of the other.
//
// It is closed by an unexported method, on the same terms as [SourceTerm]: not
// implementable outside this package, and embeddable like any Go interface. It
// is not [Term]. Term's contract is to produce an *expr.Select, which EXISTS and
// a recursive CTE need because they rewrite and compare select internals; a set
// operation is not a Select. What a
// value position needs is only a statement to nest, which expr.Subquery already
// is.
//
// It is satisfied by [Query], [SelectQuery], [ComposedQuery] and [UnionQuery] —
// every read query this package builds, because every one of them is a valid
// subquery. Whether a particular one fits a particular position is decided by
// its arity, not by its kind.
type ValueTerm interface {
	// valueTerm renders the statement to nest and reports how many columns its
	// result has. The count is the query's own construction-time metadata: the
	// select list it was built from, or — for a set operation — the result shape
	// its branches were validated against.
	valueTerm() (expr.Subquery, int, error)
}

// valueSubquery renders a value term, or reports why it could not be one.
func valueSubquery(t ValueTerm) (expr.Subquery, int, error) {
	if t == nil {
		return nil, 0, errors.New("a value subquery was given no query")
	}
	sub, arity, err := t.valueTerm()
	if err != nil {
		return nil, 0, err
	}
	if sub == nil {
		return nil, 0, errors.New("a value subquery was given a query that produced no statement")
	}
	return sub, arity, nil
}

// Exists reports whether a statement returns any row.
//
//	db.Users.Query().Where(orm.Exists[gendemo.User](
//	    orm.Rows(orm.Named("x", orm.Val(1))).
//	        From(Posts.Source()).
//	        Where(orm.Eq(Posts.AuthorID, Users.ID)),
//	))
//
// The result is a predicate and never NULL: EXISTS is TRUE or FALSE whatever
// the subquery selects. The entity parameter is free because the check that
// matters is the scope one — the subquery's references are validated against
// the sources the enclosing statement introduces, which is a stronger question
// than which entity the predicate was tagged with.
func Exists[E any](t Term) Predicate[E] { return existsOf[E](t, false) }

// NotExists reports whether a statement returns no row.
//
// It is not [InSub] negated. NOT EXISTS is FALSE as soon as one row matches and
// TRUE otherwise, with no third answer; NOT IN over a subquery that yields a
// NULL is UNKNOWN, and the difference decides whether rows come back.
func NotExists[E any](t Term) Predicate[E] { return existsOf[E](t, true) }

func existsOf[E any](t Term, not bool) Predicate[E] {
	if t == nil {
		return Predicate[E]{err: errors.New("Exists was given no query")}
	}
	sel, err := t.term()
	if err != nil {
		return Predicate[E]{err: err}
	}
	// EXISTS reads no value and no order, so the statement is reduced to the
	// rows it matches by the same canonicalisation the relation planner's
	// semi-joins already use. It is the one rewrite in this file, it is the
	// one PostgreSQL's own semantics permit, and it is shared rather than
	// repeated.
	return Predicate[E]{node: expr.Exists{Not: not, Sub: expr.ExistsProbe(sel)}}
}

// InSub builds expression IN (subquery).
//
// The subquery must return exactly one column, which is checked when the
// statement is built rather than by PostgreSQL. What is not checked here is
// that the two types are comparable: the subquery's column type is
// PostgreSQL's to decide, and it decides it precisely.
func InSub[E, T any](v Selectable[E, T], t ValueTerm) Predicate[E] {
	return inSub(v, t, false)
}

// NotInSub builds expression NOT IN (subquery).
//
// # NULL
//
// This is PostgreSQL's NOT IN, with PostgreSQL's three-valued logic, and it is
// not rewritten into NOT EXISTS. If the subquery returns a NULL, then for every
// row the comparison is UNKNOWN rather than TRUE, a WHERE clause keeps rows
// only on TRUE, and the result is no rows at all — even for values that are
// plainly not in the list.
//
// That surprises people, and rewriting it would surprise them worse: the
// rewritten statement would return rows the SQL they read does not. If the
// subquery can produce NULL and the intent is "has no match", write
// [NotExists], or exclude the NULLs in the subquery.
func NotInSub[E, T any](v Selectable[E, T], t ValueTerm) Predicate[E] {
	return inSub(v, t, true)
}

func inSub[E, T any](v Selectable[E, T], t ValueTerm, not bool) Predicate[E] {
	sub, arity, err := valueSubquery(t)
	if err != nil {
		return Predicate[E]{err: err}
	}
	// A membership test compares as many columns as the expression on its left
	// has. That expression is a Selectable, which is one column, so the subquery
	// returns one — the number is written as the left-hand side's arity rather
	// than as a constant so that a tuple left-hand side, if this package ever
	// grows one, changes it here and nowhere else.
	const left = 1
	if arity != left {
		return Predicate[E]{err: fmt.Errorf("a membership test compares %d column and the subquery returns %d", left, arity)}
	}
	return Predicate[E]{node: expr.InSubquery{X: v.selectItem().Node, Sub: sub, Not: not}}
}

// Scalar reads a statement as a value.
//
// # It is always nullable
//
// PostgreSQL's scalar subquery returns NULL when the statement matches no row,
// whatever the expression it selects. A subquery over a NOT NULL column is
// therefore nullable, and so is one selecting count(*) — the count cannot be
// NULL, but a statement returning no row at all still yields one. Claiming
// otherwise would be claiming something about the data.
//
//	latest := orm.Scalar[gendemo.User, time.Time](
//	    orm.Rows(orm.NamedNull("m", orm.Max(Posts.CreatedAt))).
//	        From(Posts.Source()).
//	        Where(...),
//	)   // Value[User, *time.Time]
//
// More than one row is a cardinality violation PostgreSQL raises at run time,
// and it arrives as a *pgconn.PgError like any other server error.
func Scalar[E, V any](t ValueTerm) Value[E, *V] {
	if t == nil {
		return Value[E, *V]{node: expr.Raw{SQL: "NULL"}}
	}
	sub, arity, err := valueSubquery(t)
	if err != nil {
		// A value has nowhere to put an error, so the mistake is carried as a
		// fragment that fails when the statement compiles. It cannot reach
		// PostgreSQL: Raw validates its own placeholders, and this one names a
		// parameter it was never given.
		return Value[E, *V]{node: failed(err)}
	}
	// A scalar subquery reads one column, and how many it has is something the
	// query has already said. PostgreSQL would refuse this too, and precisely,
	// but it would refuse it after a round trip and about SQL the caller did not
	// write down.
	//
	// How many rows it returns is a different question and not this one's. One
	// column and a thousand rows is a well-formed scalar subquery that fails at
	// run time in some positions and not in others, which is PostgreSQL's rule
	// about cardinality rather than anything about the statement's shape.
	if arity != 1 {
		return Value[E, *V]{node: failed(fmt.Errorf("a scalar subquery reads one column and this one returns %d", arity))}
	}
	return Value[E, *V]{node: expr.SubqueryValue{Sub: sub}, nullable: true}
}

// failed turns a construction mistake into a node that refuses to compile.
//
// Most of this package records mistakes in the builder, which is where they can
// be collected and reported together. An expression has no builder — it is a
// value returned from a constructor — so the mistake has to travel in the tree
// until something with an error return touches it.
func failed(err error) expr.Node { return expr.Fail{Err: err} }

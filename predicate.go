package orm

import (
	"errors"
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// Predicate is a boolean condition over entity E.
//
// The type parameter is the whole point. A Predicate[User] and a Predicate[Post]
// are different types, so combining them is a compile error and handing one to
// the wrong query is a compile error. Nothing checks entity identity at runtime
// because nothing needs to.
//
// The zero Predicate carries no condition and is ignored wherever predicates
// are combined, which is what lets a caller append conditionally without
// guarding every append.
type Predicate[E any] struct {
	node expr.Node
	// err travels with the predicate because the constructors are expressions,
	// not statements: Expr cannot return an error without breaking the shape
	// callers write predicates in. Where collects it into the builder, which is
	// where every other construction mistake already lands.
	err error
}

// IsZero reports whether the predicate carries no condition.
func (p Predicate[E]) IsZero() bool { return p.node == nil }

// Err returns the mistake made building the predicate, if any.
func (p Predicate[E]) Err() error { return p.err }

// And combines predicates so that all of them must hold.
//
// And of nothing is TRUE, which is the identity of conjunction and, more to the
// point, restricts nothing — so a query built from an empty predicate slice
// produces no WHERE clause at all rather than a syntax error. That is the case
// dynamic filtering lives in:
//
//	predicates := []orm.Predicate[User]{}
//	if filter.Email != "" {
//	    predicates = append(predicates, Users.Email.ILike("%"+filter.Email+"%"))
//	}
//	q := db.Users.Query().Where(orm.And(predicates...))
//
// With no filters set, that is SELECT ... FROM users, exactly as if Where had
// never been called.
func And[E any](ps ...Predicate[E]) Predicate[E] { return combine(expr.OpAnd, ps) }

// Or combines predicates so that at least one must hold.
//
// Or of nothing is FALSE — the identity of disjunction, and the honest answer:
// a query asked to match one of no alternatives matches nothing. Note the
// asymmetry with And, which is not an inconsistency but the same rule applied
// to a different operator.
func Or[E any](ps ...Predicate[E]) Predicate[E] { return combine(expr.OpOr, ps) }

// Not negates a predicate. Negating the zero predicate yields FALSE, since the
// zero predicate is the condition that holds of everything.
func Not[E any](p Predicate[E]) Predicate[E] {
	if p.IsZero() {
		return Predicate[E]{node: expr.Bool{Value: false}, err: p.err}
	}
	return Predicate[E]{node: expr.Unary{Op: expr.OpNot, X: p.node}, err: p.err}
}

// combine folds predicates under one boolean operator, collapsing the
// degenerate cases so that the tree never holds an empty or single-item group.
//
// An operand that is the operator's identity — TRUE under AND, FALSE under OR —
// is dropped, because it restricts nothing and so should contribute nothing to
// the text either. Without that, the ordinary shape
//
//	q.Where(orm.And(predicates...)).Where(Users.Active.Eq(true))
//
// renders as WHERE TRUE AND "users"."active" = $1 whenever the filter set is
// empty. That is correct SQL and useless reading, and SQL() exists to be read.
//
// Annihilators are left alone. An explicit FALSE under AND stays visible,
// because a caller who wrote In() over an empty slice is better served seeing
// why nothing matches than seeing the whole clause vanish.
func combine[E any](op expr.Op, ps []Predicate[E]) Predicate[E] {
	identity := op == expr.OpAnd
	items := make([]expr.Node, 0, len(ps))
	var errs []error
	for _, p := range ps {
		// A mistake in an operand is a mistake in the combination. Dropping it
		// here would let a malformed fragment disappear into an And and take
		// its condition with it.
		if p.err != nil {
			errs = append(errs, p.err)
		}
		if p.IsZero() {
			continue
		}
		if b, ok := p.node.(expr.Bool); ok && b.Value == identity {
			continue
		}
		items = append(items, p.node)
	}
	out := Predicate[E]{err: errors.Join(errs...)}
	switch len(items) {
	case 0:
		out.node = expr.Bool{Value: op == expr.OpAnd}
	case 1:
		out.node = items[0]
	default:
		out.node = expr.Group{Op: op, Items: items}
	}
	return out
}

// Expr is the escape hatch: a predicate written as SQL.
//
// It exists because PostgreSQL is larger than any typed API, and a query
// builder that cannot express something the database can is a builder people
// abandon. What it gives up is the type checking — the fragment is not
// validated against the entity's columns — and nothing else. The fragment's own
// placeholders are renumbered into the surrounding statement and its arguments
// join the parameter list, so values are still never part of the SQL text:
//
//	orm.Expr[User]("score > $1", 100)
//
// alongside a typed predicate compiles to
//
//	WHERE "users"."active" = $1 AND score > $2
//
// with both values bound. Placeholders are checked against the arguments when
// the predicate is built: a fragment referring to $2 with one argument, or
// given an argument it never refers to, carries [ErrRawPlaceholder] and fails
// the query before it reaches PostgreSQL. A local placeholder used twice binds
// one argument, as it would in a statement written by hand.
//
// Reach for a typed predicate first. This is for what they cannot say.
func Expr[E any](sql string, args ...any) Predicate[E] {
	refs, err := expr.ScanPlaceholders(sql)
	if err != nil {
		return Predicate[E]{err: fmt.Errorf("%w: %w", ErrRawPlaceholder, err)}
	}
	if err := expr.ValidatePlaceholders(refs, len(args)); err != nil {
		return Predicate[E]{err: err}
	}
	return Predicate[E]{node: expr.Raw{SQL: sql, Args: args}}
}

// Order is one ORDER BY term over entity E, produced by a column's Asc or Desc.
// Like Predicate, it carries its entity so that ordering a query by another
// entity's column does not compile.
type Order[E any] struct {
	order expr.Order
}

// IsZero reports whether the term carries nothing to order by.
func (o Order[E]) IsZero() bool { return o.order.Expr == nil && o.order.Column.Name == "" }

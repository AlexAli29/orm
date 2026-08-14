package orm

import (
	"errors"
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// Common table expressions.
//
// A CTE is a query given a name in front of the statement that uses it, and a
// reference to it is a [Source] like a table or a derived table. That is not a
// simplification: it is the whole implementation. There is no CTE join path and
// no CTE scope, because everything that takes a source already takes this one,
// and the output columns are declared the same way a derived table's are.
//
//	active := orm.CTE("active_users", orm.Rows(id, email).
//	    From(Users.Source()).
//	    Where(orm.Cond(Users.Active.Eq(true))))
//
//	orm.Compose(ex, shape).With(active).From(active)
//
// The name is written as a quoted identifier, so a CTE called "users" shadows
// the table of that name inside this statement and nowhere else — which is
// PostgreSQL's rule, and is why the two are never confused: a table source
// writes its schema-qualified name and a CTE reference writes the bare one.

// CTE names a row-producing query so that a statement can select from it.
//
// The returned value is both the declaration, which [ComposedQuery.With]
// renders, and the reference, which From and the joins take. Aliasing it with
// As returns a second reference to the same item, which is how one CTE is
// joined to itself.
func CTE(name string, t SourceTerm, opts ...CTEOption) *Source {
	sub, outs, err := readSource(t)
	if err != nil {
		return expr.FailedCTE(name, false, err)
	}
	out := expr.NewCTE(name, sub, outs)
	for _, o := range opts {
		o.apply(out)
	}
	return out
}

// CTEOption configures a WITH item as it is declared.
//
// It is an option rather than a transformation because a source's identity is
// its pointer: a function returning a hinted copy would return a second source,
// and every expression built from the first would be out of scope in the
// statement that used the second.
type CTEOption interface{ apply(*Source) }

type materializeOpt bool

func (m materializeOpt) apply(s *Source) {
	expr.SetMaterialized(s, bool(m))
}

// Materialized asks PostgreSQL to evaluate the CTE once and keep its rows.
//
// It is a hint about execution rather than about meaning, and it is offered
// because the default changed: before PostgreSQL 12 a CTE was always an
// optimisation fence, and since then the planner may inline one. This asks for
// the older behaviour, which is worth having for a body that is expensive and
// referenced twice.
const Materialized = materializeOpt(true)

// NotMaterialized asks PostgreSQL to inline the CTE into the query using it, so
// that the outer query's conditions can reach inside it.
const NotMaterialized = materializeOpt(false)

// RecursiveCTE names a query that refers to itself.
//
// It is the one place M11 composes two rowsets, and the shape is PostgreSQL's:
// an anchor term that produces the starting rows, then a recursive term that is
// evaluated against what the previous round produced, until a round produces
// nothing. The builder gives the recursive term a source referring to the CTE
// itself, because that source has to exist before the statement defining it is
// finished:
//
//	tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
//	    return orm.Rows(id, managerID).
//	        From(Users.Source()).
//	        Join(self, orm.Eq(Users.ManagerID, ...))
//	})
//
// The output columns and their types come from the anchor term, and the
// recursive term is checked against them: a different arity, a different set of
// names, or a recursive term that can produce NULL where the anchor cannot, are
// all refused. PostgreSQL checks the column types itself, and precisely.
//
// The two terms are combined with UNION ALL, which is what a walk over a tree
// wants. Use [RecursiveCTEUnion] for a graph that may contain a cycle: UNION
// removes rows already produced, which is what makes such a query terminate.
//
// Termination is the caller's. Nothing here inspects the recursive term for a
// stopping condition, and nothing counts iterations in Go — PostgreSQL runs the
// query, and a query that does not terminate is a query that does not terminate.
func RecursiveCTE(name string, anchor Term, recursive func(self *Source) Term) *Source {
	return recursiveCTE(name, anchor, recursive, true)
}

// RecursiveCTEUnion is [RecursiveCTE] with UNION instead of UNION ALL, so that
// a row already produced is not produced again.
func RecursiveCTEUnion(name string, anchor Term, recursive func(self *Source) Term) *Source {
	return recursiveCTE(name, anchor, recursive, false)
}

func recursiveCTE(name string, anchor Term, recursive func(self *Source) Term, all bool) *Source {
	fail := func(err error) *Source {
		return expr.FailedCTE(name, true, err)
	}
	if recursive == nil {
		return fail(fmt.Errorf("recursive CTE %q has no recursive term", name))
	}
	anchorSel, outs, err := source(anchor)
	if err != nil {
		return fail(err)
	}
	// The self-reference is built from the anchor's declarations, so the
	// recursive term addresses the same columns by the same names and at the
	// same types the enclosing statement will.
	self := expr.NewCTERef(name, outs)
	if self.Err() != nil {
		return fail(self.Err())
	}
	recursiveSel, _, err := source(recursive(self))
	if err != nil {
		return fail(err)
	}
	if err := expr.ValidateRecursive(name, anchorSel, recursiveSel); err != nil {
		return fail(err)
	}
	out := expr.NewCTE(name, &expr.SetOp{Left: anchorSel, Right: recursiveSel, All: all}, outs)
	expr.SetRecursive(out)
	return out
}

// WriteTerm is a write whose RETURNING clause produces rows.
//
// It is what makes a data-modifying WITH item possible, and it is satisfied by
// the write builders' returning forms. The rows are the ones the write touched,
// which is a question only the write can answer — reading them back with a
// separate SELECT would be a different query against a database that has since
// moved on.
type WriteTerm interface {
	// writeTerm renders the statement's tree and reports its output names.
	writeTerm() (expr.Subquery, []string, error)
}

// WritingCTE names a data-modifying statement so that a query can select from
// the rows it touched.
//
//	moved := orm.WritingCTE("moved", orm.UpdateReturning(u, shape))
//	orm.Compose(ex, report).With(moved).From(moved)
//
// PostgreSQL runs such an item exactly once, whether or not the main query
// reads its rows, and every part of the statement sees the same snapshot: rows
// the item changed are visible to the rest of the statement only through the
// item's own output. That is worth knowing before writing one, and it is not
// something this package can soften.
//
// Every column of the RETURNING list has to be named, because a column of a CTE
// is addressed by name. Alias them with As, as a derived table's are.
func WritingCTE(name string, w WriteTerm) *Source {
	if w == nil {
		return expr.FailedCTE(name, false, errors.New("a data-modifying CTE was given no statement"))
	}
	body, outs, err := w.writeTerm()
	if err != nil {
		return expr.FailedCTE(name, false, err)
	}
	return expr.NewCTE(name, body, outs)
}

// writeTerm makes a write with a RETURNING clause usable as a WITH item.
func (t *Returning[E, R]) writeTerm() (expr.Subquery, []string, error) {
	if len(t.errs) > 0 {
		return nil, nil, errors.Join(t.errs...)
	}
	sub, ok := t.stmt.(expr.Subquery)
	if !ok {
		return nil, nil, fmt.Errorf("a %s cannot be nested", t.what)
	}
	outs := make([]string, 0, len(t.proj.items))
	for i, it := range t.proj.items {
		if it.Alias == "" {
			return nil, nil, fmt.Errorf("returned column %d has no name; a column of a CTE is addressed by name, so every one of them is aliased", i+1)
		}
		outs = append(outs, it.Alias)
	}
	return sub, outs, nil
}

// With declares the statement's WITH items, in the order given.
//
// A later item may name an earlier one; an earlier one may not name a later
// one, and only a recursive item may name itself. All three are checked when
// the statement is built, against the item rather than against a relation
// PostgreSQL could not resolve.
//
// The items are written before the rest of the statement, so their parameters
// are numbered first — one statement, one parameter list, in the order the SQL
// reads.
func (q *ComposedQuery[R]) With(ctes ...*Source) *ComposedQuery[R] {
	for i, c := range ctes {
		if err := validWithItem(i, c); err != nil {
			q.fail(err)
			continue
		}
		q.with = append(q.with, c)
	}
	return q
}

// validWithItem reports why a source cannot be declared as a WITH item.
//
// Two builders take WITH items and they take the same ones, so the rule is
// written once. A reference is the case worth naming separately: a CTE value is
// both the declaration and the reference, and passing the reference form to With
// declares nothing.
func validWithItem(i int, c *Source) error {
	switch {
	case c == nil:
		return fmt.Errorf("WITH item %d is missing", i+1)
	case c.Err() != nil:
		return c.Err()
	case !c.IsCTE():
		return fmt.Errorf("With takes named queries; %s is not one", c)
	case !c.HasDefinition():
		return fmt.Errorf("WITH item %q has no statement; it is a reference to one declared elsewhere", c.Name())
	}
	return nil
}

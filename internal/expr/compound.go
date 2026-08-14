package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// Set-composed statements.
//
// A compound is two or more SELECTs combined by a set operation. It is a
// Subquery like every other statement here, which is not a detail: Subquery is
// what a derived source and a WITH item hold, so a compound becomes usable as
// both without either of them learning what a compound is.
//
// It shares the writer, and therefore the parameter numbering. A branch is
// written into the same builder with the same arg slice, so the placeholders of
// the second branch continue the first's rather than restarting — which is the
// one thing about compiling a compound that cannot be got wrong quietly, since
// restarting them produces valid SQL that binds the wrong values.
//
// v1 renders one operator. The field exists because the node has to say which
// operation it is somewhere, and a bare boolean would say it worse; it does not
// exist as a place to add UNION, INTERSECT and EXCEPT by filling in constants.
// Those are not part of this milestone and the writer refuses anything else.

// SetKind is the set operation a compound performs.
//
// SetOp is already taken, by the node between a recursive CTE's anchor and its
// recursive term. That one exists where PostgreSQL's grammar requires it and
// nowhere else; this one is the free-standing composition, and they are
// deliberately not merged — the recursive node's UNION is part of a WITH
// RECURSIVE and answers none of the questions about column names, types and
// nullability that a free-standing composition has to.
type SetKind uint8

const (
	setKindInvalid SetKind = iota
	// SetUnionAll concatenates the branches, keeping duplicate rows. It is the
	// only free-standing set operation this package compiles.
	SetUnionAll
)

func (o SetKind) String() string {
	if o == SetUnionAll {
		return "UNION ALL"
	}
	return "invalid set operation"
}

// Compound is a set operation over two or more branches.
//
// The branches are a slice rather than a left and a right because A UNION ALL B
// UNION ALL C is one operation over three inputs, and modelling it as a tree of
// pairs would make the SQL depend on which way a caller happened to associate
// it. Nesting is still expressible — a branch may itself be a Compound — and
// then it is parenthesised, because at that point the caller asked for it.
type Compound struct {
	Op SetKind
	// With holds the compound's own WITH items. They are written before the
	// first branch and are visible to every branch, which is PostgreSQL's rule
	// and not a choice made here.
	With     []*Source
	Branches []Subquery
	// OrderBy, Limit and Offset apply to the whole result. PostgreSQL attaches
	// them to the compound rather than to the last branch, and the difference
	// is the entire reason they live on this node instead of being pushed down.
	OrderBy []OutputOrder
	Limit   *int
	Offset  *int
}

// OutputOrder is one ORDER BY term of a set operation.
//
// It names an output column and nothing else, because that is the whole of what
// PostgreSQL's grammar allows there. A qualified reference is rejected —
//
//	ERROR: missing FROM-clause entry for table "t"
//
// and so is any expression, even one over an output name:
//
//	ERROR: invalid UNION/INTERSECT/EXCEPT ORDER BY clause
//
// So this is a name and a direction rather than an [Order], which carries a node
// and could hold either of those. A compound cannot be given an ordering term it
// is unable to render, because there is nowhere to put one.
//
// The name is the compound's own output name, which PostgreSQL takes from the
// first branch. Whether a particular name is one of them is decided by the layer
// that knows the result shape, before a Compound is built.
type OutputOrder struct {
	Name string
	Desc bool
}

// Compile renders the statement and its parameters.
func (c *Compound) Compile() (string, []any, error) { return compileAlone(c) }

// write renders the compound into an existing writer.
func (c *Compound) write(w *writer) error {
	switch {
	case c.Op != SetUnionAll:
		return fmt.Errorf("compound statement has %s; v1 compiles UNION ALL and nothing else", c.Op)
	case len(c.Branches) < 2:
		return fmt.Errorf("a compound statement needs at least two branches, and this one has %d", len(c.Branches))
	case c.Limit != nil && *c.Limit < 0:
		return fmt.Errorf("negative limit %d", *c.Limit)
	case c.Offset != nil && *c.Offset < 0:
		return fmt.Errorf("negative offset %d", *c.Offset)
	}
	for i, b := range c.Branches {
		if b == nil {
			return fmt.Errorf("branch %d of the compound statement is empty", i+1)
		}
		if err := ValidateBranch(b); err != nil {
			return fmt.Errorf("branch %d: %w", i+1, err)
		}
	}

	if w.scope.Depth() >= MaxDepth {
		return fmt.Errorf("statements are nested more than %d deep, which is past the point where a query is a mistake rather than a request", MaxDepth)
	}

	// The compound's own frame carries its WITH items and nothing else. Each
	// branch pushes a frame of its own on top, so a branch sees the compound's
	// CTEs and its own sources — and not the other branch's, which is what
	// makes "a branch is not a scope-sharing mechanism" structural rather than
	// a rule somebody has to remember.
	w.scope.Push()
	defer w.scope.Pop()

	popWith, err := w.writeWith(c.With)
	defer popWith()
	if err != nil {
		return err
	}

	for i, b := range c.Branches {
		if i > 0 {
			w.b.WriteString(" ")
			w.b.WriteString(c.Op.String())
			w.b.WriteString(" ")
		}
		if err := c.writeBranch(w, b); err != nil {
			// Nesting is not re-announced. A compound past the depth limit would
			// otherwise report the one sentence that says what went wrong behind
			// one "branch 1: " per level, which is a diagnostic nobody reads to
			// the end.
			if strings.HasPrefix(err.Error(), "branch ") {
				return err
			}
			return fmt.Errorf("branch %d: %w", i+1, err)
		}
	}

	if len(c.OrderBy) > 0 {
		w.b.WriteString(" ORDER BY ")
		for i, o := range c.OrderBy {
			if o.Name == "" {
				return fmt.Errorf("ordering term %d of the compound statement names no output column", i+1)
			}
			if i > 0 {
				w.b.WriteString(", ")
			}
			// A bare identifier, not a qualified one: it names a column of the
			// compound's own result, which belongs to no source and is not in
			// any scope the writer tracks.
			w.ident(o.Name)
			if o.Desc {
				w.b.WriteString(" DESC")
			} else {
				w.b.WriteString(" ASC")
			}
		}
	}
	if c.Limit != nil {
		w.b.WriteString(" LIMIT ")
		w.b.WriteString(strconv.Itoa(*c.Limit))
	}
	if c.Offset != nil {
		w.b.WriteString(" OFFSET ")
		w.b.WriteString(strconv.Itoa(*c.Offset))
	}
	return nil
}

// writeBranch renders one branch, in parentheses when the grammar needs them.
func (c *Compound) writeBranch(w *writer, b Subquery) error {
	if !needsParens(b) {
		return b.write(w)
	}
	w.b.WriteString("(")
	if err := b.write(w); err != nil {
		return err
	}
	w.b.WriteString(")")
	return nil
}

// needsParens reports whether a branch has to be wrapped.
//
// PostgreSQL's grammar attaches ORDER BY, LIMIT and OFFSET to the compound, not
// to the branch that appears to carry them. Written bare,
//
//	SELECT ... ORDER BY x LIMIT 2 UNION ALL SELECT ...
//
// is either a syntax error or a statement that orders and limits the whole
// union — never the branch-local one the caller asked for. Parenthesising is
// what makes the branch mean what it says.
//
// A WITH clause is the same problem and reads worse, because the bare form is
// accepted:
//
//	WITH x AS (...) SELECT ... FROM x UNION ALL SELECT ...
//
// declares x for the whole compound. EXPLAIN puts the CTE above the Append: it
// is evaluated once for the operation and is visible to every branch, which is
// not the branch-local declaration that was written. In any later branch the
// bare form is a syntax error instead, so the same omission fails loudly in one
// position and silently in the other.
//
// A nested compound is wrapped for the same reason: the inner one's own ORDER
// BY and LIMIT would otherwise bind to the outer.
//
// Locking clauses are deliberately not here. Parentheses do not make them legal
// and nothing does; see [ValidateBranch].
func needsParens(b Subquery) bool {
	switch s := b.(type) {
	case *Select:
		return len(s.With) > 0 || len(s.OrderBy) > 0 || s.Limit != nil || s.Offset != nil
	case *Compound:
		return true
	default:
		// An unknown statement kind is wrapped, because the safe answer to "is
		// this self-delimiting" is no.
		return true
	}
}

// ValidateBranch reports why a statement cannot be a branch of a compound.
//
// One thing can be wrong that parentheses do not fix. PostgreSQL refuses a
// locking clause anywhere in a set operation:
//
//	(SELECT ... FOR UPDATE) UNION ALL SELECT ...
//	ERROR: FOR UPDATE is not allowed with UNION/INTERSECT/EXCEPT
//
// and refuses it on the compound itself for the same reason. Row locking is
// defined over the rows a query reads from a table, and a concatenation's rows
// are not those rows — so the clause has no meaning here rather than an
// inconvenient one.
//
// It is a refusal rather than a rewrite because there is nothing to rewrite it
// into: dropping the clause would run an unlocked query the caller believes is
// locked, and hoisting it would lock rows nobody named.
//
// It is exported so that the layer assembling a compound can refuse a branch
// when it is handed one, rather than when the statement is written. The writer
// asks as well, which is the floor for a tree assembled inside this package.
func ValidateBranch(s Subquery) error {
	sel, ok := s.(*Select)
	if !ok {
		return nil
	}
	switch {
	case sel.ForUpdate:
		return fmt.Errorf("this branch locks the rows it reads (FOR UPDATE), and PostgreSQL does not allow a locking clause in a set operation; parenthesising the branch does not help, because a concatenation has no table rows to lock")
	case sel.Lock.Strength != LockNone:
		return fmt.Errorf("this branch locks the rows it reads (%s), and PostgreSQL does not allow a locking clause in a set operation; parenthesising the branch does not help, because a concatenation has no table rows to lock", sel.Lock.Strength)
	}
	return nil
}

// bound reports the sources the compound introduces.
//
// A branch's own FROM items are bound inside that branch and are not visible
// outside it, so what a compound binds is its WITH items alone.
func (c *Compound) bound(add func(*Source)) {
	for _, s := range c.With {
		add(s)
	}
}

// free reports the sources the compound refers to but does not introduce.
//
// A source is free for the compound when it is free for a branch and the
// compound does not itself bind it. That is what makes a correlated reference
// inside a branch correlate to the query enclosing the compound, and a
// reference to the other branch's source unresolvable.
func (c *Compound) free(add func(*Source)) {
	own := make(map[*Source]bool, len(c.With))
	c.bound(func(s *Source) { own[s] = true })
	for _, b := range c.Branches {
		b.free(func(s *Source) {
			if !own[s] {
				add(s)
			}
		})
	}
	// The ordering terms name output columns rather than sources, so there is
	// nothing in them to be free.
}

// resultArity reports the shared arity of the branches, or -1 when they
// disagree.
//
// A disagreement is reported as "unknown" rather than as one of the answers, so
// that whatever is checking — a scalar subquery, an IN, a WITH item — reports it
// with the names attached rather than silently taking the first branch's word.
func (c *Compound) resultArity() int {
	arity := -1
	for i, b := range c.Branches {
		n := b.resultArity()
		if i == 0 {
			arity = n
			continue
		}
		if n != arity {
			return -1
		}
	}
	return arity
}

// each reports every source appearing anywhere in the compound.
func (c *Compound) each(add func(*Source)) {
	for _, s := range c.With {
		add(s)
		if s.sub != nil {
			s.sub.each(add)
		}
	}
	for _, b := range c.Branches {
		b.each(add)
	}
}

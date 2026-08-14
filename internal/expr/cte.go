package expr

import "fmt"

// WITH.
//
// A CTE is a named statement declared in front of the one that uses it, and a
// reference to it is a [Source] like any other. That is the whole design: there
// is no CTE join path, no CTE scope and no CTE compiler, because a WITH item
// that has been named is a row source and every clause that takes a row source
// already takes it.
//
// The definition and the reference are the same value for the same reason. A
// source of kind SourceCTE carries the body when it is the item and nothing
// when it is only a reference; the FROM clause writes the name either way, and
// the WITH clause is the one place the body is written.
//
// What is specific to WITH is declaration order. A later item may name an
// earlier one; an earlier one may not name a later one, and only a recursive
// item may name itself. PostgreSQL enforces that too, but by then the statement
// is text and the message is about a relation that does not exist.

// SetOp is a set operation over two statements.
//
// M11 does not add a general UNION: composing arbitrary rowsets raises questions
// about column names, types and nullability that a recursive CTE answers by
// construction and a free-standing UNION does not. So this node exists in the
// one place PostgreSQL's grammar requires it — between a recursive CTE's anchor
// and its recursive term — and the builder above only ever produces it there.
type SetOp struct {
	Left, Right Subquery
	// All selects UNION ALL, which is what a recursive walk usually wants:
	// UNION removes duplicates on every iteration, which is a different query
	// and a slower one — but it is also what stops a cyclic graph, so both are
	// offered above.
	All bool
}

// write renders the operation, sharing the enclosing writer's parameters.
func (s *SetOp) write(w *writer) error {
	if s.Left == nil || s.Right == nil {
		return fmt.Errorf("a set operation needs two statements")
	}
	if err := s.Left.write(w); err != nil {
		return err
	}
	if s.All {
		w.b.WriteString(" UNION ALL ")
	} else {
		w.b.WriteString(" UNION ")
	}
	return s.Right.write(w)
}

func (s *SetOp) bound(add func(*Source)) {
	s.Left.bound(add)
	s.Right.bound(add)
}

// free reports what the operation needs from an enclosing query.
//
// The two terms are independent statements, so a source one of them binds does
// not satisfy the other's reference to it. Taking the union of the two free
// sets rather than subtracting either side's bound set is what keeps that true.
func (s *SetOp) free(add func(*Source)) {
	s.Left.free(add)
	s.Right.free(add)
}

func (s *SetOp) each(add func(*Source)) {
	s.Left.each(add)
	s.Right.each(add)
}

// resultArity reports the shared arity of the two terms, or -1 when they
// disagree. A disagreement is a mistake, and reporting it as "unknown" lets the
// one place that validates a WITH item report it with the names attached.
func (s *SetOp) resultArity() int {
	l, r := s.Left.resultArity(), s.Right.resultArity()
	if l != r {
		return -1
	}
	return l
}

// writeWith renders the WITH clause and checks its declaration order.
// It returns the function that takes the declared names out of force again, so
// that the caller's defer covers the whole statement the clause belongs to. The
// function is never nil, including when there is no clause.
func (w *writer) writeWith(items []*Source) (func(), error) {
	if len(items) == 0 {
		// Nothing comes into force and nothing has to go out again. This is
		// where most statements go — a WITH clause is the exception — and it is
		// on the path every compile takes, so it allocates nothing: no map to
		// hold names that do not exist, no frame to push, and a pop that is a
		// static function value rather than a closure over this writer.
		return noNames, nil
	}
	inForce := make(map[string]bool, len(items))
	// The names come into force before any body is written, so a recursive item
	// may refer to itself.
	for _, c := range items {
		if c != nil {
			inForce[c.cTEName] = true
		}
	}
	pop := w.pushWith(inForce)
	declared := make(map[string]bool, len(items))
	recursive := false
	for _, c := range items {
		if c != nil && c.recursive {
			recursive = true
		}
	}

	w.b.WriteString("WITH ")
	// RECURSIVE is a property of the whole clause rather than of the item that
	// needs it: PostgreSQL's grammar puts the keyword once, after WITH, and it
	// then permits — but does not require — self-reference in any item.
	if recursive {
		w.b.WriteString("RECURSIVE ")
	}
	for i, c := range items {
		if err := validateCTE(c, declared); err != nil {
			return pop, err
		}
		declared[c.cTEName] = true

		if i > 0 {
			w.b.WriteString(", ")
		}
		w.ident(c.cTEName)
		// A recursive item takes its column names from its first term, which
		// is a fact about PostgreSQL rather than about what the caller
		// declared. Writing the list makes the names the declaration's.
		if c.recursive {
			w.b.WriteByte('(')
			for j, col := range c.outputs {
				if j > 0 {
					w.b.WriteString(", ")
				}
				w.ident(col)
			}
			w.b.WriteByte(')')
		}
		w.b.WriteString(" AS ")
		if c.materialized != nil {
			if *c.materialized {
				w.b.WriteString("MATERIALIZED ")
			} else {
				w.b.WriteString("NOT MATERIALIZED ")
			}
		}
		w.b.WriteByte('(')
		if err := c.sub.write(w); err != nil {
			return pop, err
		}
		w.b.WriteByte(')')
	}
	w.b.WriteByte(' ')
	return pop, nil
}

// validateCTE refuses an item that cannot be resolved.
//
// The reference check is what makes declaration order mean something. Every CTE
// source mentioned anywhere inside the body — whether it is selected from or
// correlated to — has to name an item already declared, or this one when it is
// recursive. PostgreSQL agrees, and says so about a relation that does not
// exist; saying it here names the WITH item instead.
func validateCTE(c *Source, declared map[string]bool) error {
	switch {
	case c == nil:
		return fmt.Errorf("a WITH item is missing")
	case c.kind != SourceCTE:
		return fmt.Errorf("a WITH item has to be a named query; %s is not one", c)
	case c.cTEName == "":
		return fmt.Errorf("a WITH item has no name")
	case c.sub == nil:
		return fmt.Errorf("WITH item %q has no statement; it is a reference rather than a declaration", c.cTEName)
	case c.aliasErr != nil:
		return c.aliasErr
	case declared[c.cTEName]:
		return fmt.Errorf("WITH item %q is declared twice; a reference to it would name whichever PostgreSQL resolved first", c.cTEName)
	}

	var bad *Source
	c.sub.each(func(s *Source) {
		if bad != nil || s.kind != SourceCTE {
			return
		}
		if declared[s.cTEName] || (c.recursive && s.cTEName == c.cTEName) {
			return
		}
		bad = s
	})
	if bad == nil {
		return nil
	}
	if bad.cTEName == c.cTEName {
		return fmt.Errorf("WITH item %q refers to itself but was not declared recursive", c.cTEName)
	}
	return fmt.Errorf("WITH item %q refers to %q, which is declared after it; a WITH item can only name the ones declared before it", c.cTEName, bad.cTEName)
}

// ValidateRecursive checks that a recursive CTE's two terms agree.
//
// PostgreSQL checks the types, and checks them precisely. What it cannot check
// is the claim this package makes about them: the output declarations come from
// the anchor, so a recursive term that can produce NULL where the anchor cannot
// would leave a result typed as non-nullable and a row that is NULL. That is
// the one mismatch worth refusing here, because it is the one PostgreSQL will
// happily accept.
func ValidateRecursive(name string, anchor, recursive *Select) error {
	switch {
	case anchor == nil || recursive == nil:
		return fmt.Errorf("recursive WITH item %q needs an anchor term and a recursive term", name)
	case len(anchor.Items) != len(recursive.Items):
		return fmt.Errorf("recursive WITH item %q selects %d columns in its anchor term and %d in its recursive term",
			name, len(anchor.Items), len(recursive.Items))
	}
	for i := range anchor.Items {
		a, r := anchor.Items[i], recursive.Items[i]
		if a.Alias != r.Alias {
			return fmt.Errorf("recursive WITH item %q names column %d %q in its anchor term and %q in its recursive term; PostgreSQL takes the names from the anchor, so the two would disagree about what was selected",
				name, i+1, a.Alias, r.Alias)
		}
		if r.Nullable && !a.Nullable {
			return fmt.Errorf("recursive WITH item %q types column %q from its anchor term, which cannot be NULL, but its recursive term can produce one; declare that output with NamedNull so the result can hold it",
				name, a.Alias)
		}
	}
	return nil
}

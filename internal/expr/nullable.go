package expr

import (
	"fmt"
	"strings"
)

// Source-induced nullability.
//
// A column's own constraint is not the last word on whether reading it can
// produce NULL. Put a NOT NULL column on the right of a LEFT JOIN and every row
// with no match returns NULL for it — not because the column changed, but
// because the source it belongs to can be absent from the joined row.
//
// So nullability has two halves. The intrinsic half belongs to the column and
// is settled by reconciliation before a query exists. The other half belongs to
// the query, and this file is where the compiler works it out: which sources an
// outer join can leave empty, and therefore which select-list items have to be
// read into something that can hold a NULL.
//
// The check refuses rather than repairs. Widening the destination silently
// would mean the Go type of a result depending on a join written three lines
// later, which is exactly the kind of action at a distance the typed layer
// exists to prevent.

// nullableSources reports which of the statement's own sources an outer join
// can leave with no row.
//
// The FROM clause this package builds is a list, so the join tree is left-deep
// by construction: each join attaches one source to everything introduced
// before it. That is what makes the rule a fold rather than a tree walk —
// LEFT makes the attached source nullable, RIGHT makes everything to its left
// nullable, FULL does both, and INNER and CROSS change nothing. A join tree
// with parentheses would need more, and there is no way to build one: a
// parenthesised join is a derived table, which is a scope of its own.
func (s *Select) nullableSources() map[*Source]bool {
	if len(s.Joins) == 0 {
		return nil
	}
	nullable := make(map[*Source]bool)
	left := make([]*Source, 0, len(s.Joins)+1)
	if s.From != nil {
		left = append(left, s.From)
	}
	for _, j := range s.Joins {
		switch j.Kind {
		case JoinLeft:
			nullable[j.Source] = true
		case JoinRight:
			for _, src := range left {
				nullable[src] = true
			}
		case JoinFull:
			for _, src := range left {
				nullable[src] = true
			}
			nullable[j.Source] = true
		}
		// A source attached by an outer join keeps the nullability it already
		// had: joining C to a left-joined B does not make B any less absent.
		left = append(left, j.Source)
	}
	return nullable
}

// validateNullability refuses a select list that reads a column of a source an
// outer join can leave empty into a destination that cannot hold NULL.
//
// An aggregate is a barrier. count(b.id) over a LEFT JOIN is zero rather than
// NULL for a row with no match, and min(b.score) is NULL for reasons that have
// nothing to do with the join — either way the aggregate's own result type is
// the answer, and the column inside it is not being read row by row.
func (s *Select) validateNullability() error {
	nullable := s.nullableSources()
	if len(nullable) == 0 {
		return nil
	}
	for i, it := range s.Items {
		if it.Nullable || it.Node == nil {
			continue
		}
		found := absorbedNull(it.Node, nullable)
		if found != nil {
			return &NullabilityError{Item: i, Alias: it.Alias, Source: found}
		}
	}
	return nil
}

// absorbedNull reports the first outer-joinable source a node reads as a value,
// or nil when every such read is behind something that handles the NULL.
//
// Two constructs handle it. An aggregate collapses rows, so its result type is
// its own business — count(b.id) over a LEFT JOIN is zero rather than NULL, and
// min(b.score) is NULL for reasons that have nothing to do with the join.
// COALESCE handles it by definition: it is NULL only if its last argument is,
// because every earlier one it would have returned instead. Checking that last
// argument and nothing else is the sound reading, and it is what makes
// coalesce(s.count, 0) a non-nullable result rather than a refusal.
func absorbedNull(n Node, nullable map[*Source]bool) *Source {
	switch n := n.(type) {
	case nil:
		return nil
	case Column:
		if n.Source != nil && nullable[n.Source] {
			return n.Source
		}
		return nil
	case Aggregate:
		return nil
	case Call:
		if n.Func == "coalesce" && len(n.Args) > 0 {
			return absorbedNull(n.Args[len(n.Args)-1], nullable)
		}
	case Case:
		// A branch condition cannot make the result NULL: a condition that is
		// UNKNOWN because the source was absent simply does not match, and the
		// next branch or the ELSE decides the value. Only the values a branch
		// can produce are read as the result.
		for _, br := range n.When {
			if found := absorbedNull(br.Then, nullable); found != nil {
				return found
			}
		}
		return absorbedNull(n.Else, nullable)
	}
	var found *Source
	children(n, func(child Node) {
		if found == nil {
			found = absorbedNull(child, nullable)
		}
	})
	return found
}

// NullabilityError reports a result that would have to hold a NULL the join can
// produce and has no way to.
type NullabilityError struct {
	// Item is the position of the select-list entry, counting from zero.
	Item int
	// Alias is its result name, when it has one.
	Alias  string
	Source *Source
}

func (e *NullabilityError) Error() string {
	var b strings.Builder
	what := fmt.Sprintf("select-list expression %d", e.Item+1)
	if e.Alias != "" {
		what = fmt.Sprintf("%s (%q)", what, e.Alias)
	}
	fmt.Fprintf(&b, "%s reads %s, which an outer join can leave with no row, "+
		"into a result that cannot hold NULL", what, e.Source)
	b.WriteString("\n\nan outer join makes every value of that source nullable, whatever the column's own constraint says;" +
		" read it with Opt or OptRef, which widen the result type to match")
	return b.String()
}

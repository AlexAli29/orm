package expr

import (
	"fmt"
	"strings"
)

// Scope is the set of table occurrences a statement may refer to.
//
// It exists so that a column reference can be checked rather than assumed. A
// predicate built from an alias the query never selects from compiles to SQL
// that names a table not in the FROM clause, and PostgreSQL's complaint about
// it arrives late and in its own vocabulary. The scope catches it here, in the
// caller's.
//
// Frames are a stack because a subquery sees its own sources and the ones
// enclosing it. M3 pushes exactly one frame; the shape is what makes that
// remain true rather than accidental when more arrive.
type Scope struct {
	frames []frame
}

type frame struct {
	sources map[*Source]bool
	// byAlias catches two occurrences claiming one name in a single frame,
	// where the second would silently shadow the first.
	byAlias map[string]*Source
	order   []*Source
}

// MaxDepth caps how deeply statements may nest.
//
// Correlation is genuinely recursive — a subquery may correlate to the query
// above it, which may correlate to the one above that — and the scope has no
// natural stopping point, so the compiler needs one. The limit is far past any
// query somebody meant to write and far short of a stack overflow, which is the
// only failure a limitless version could produce.
const MaxDepth = 32

// Push begins a new frame.
func (s *Scope) Push() {
	s.frames = append(s.frames, frame{
		sources: make(map[*Source]bool),
		byAlias: make(map[string]*Source),
	})
}

// Depth reports how many frames are open.
func (s *Scope) Depth() int { return len(s.frames) }

// Pop ends the innermost frame.
//
// A subquery's sources go out of scope when the subquery does. Leaving them
// behind would make a column of the subquery's table legal in the clause after
// it, which is exactly the mistake this type exists to catch.
func (s *Scope) Pop() {
	if len(s.frames) > 0 {
		s.frames = s.frames[:len(s.frames)-1]
	}
}

// Add makes src visible in the innermost frame.
func (s *Scope) Add(src *Source) error {
	if src == nil {
		return fmt.Errorf("a statement selects from no table")
	}
	if src.aliasErr != nil {
		return src.aliasErr
	}
	if len(s.frames) == 0 {
		s.Push()
	}
	f := &s.frames[len(s.frames)-1]
	// One occurrence introduced twice is the same ambiguity two occurrences of
	// one name are, and it is easier to write by accident: passing a source to
	// both From and a join, or joining a table descriptor to itself without
	// aliasing one side. PostgreSQL refuses it; refusing it here names the
	// occurrence rather than the table.
	if f.sources[src] {
		return &AliasCollisionError{Alias: src.Ref(), First: src, Second: src}
	}
	if other, ok := f.byAlias[src.Ref()]; ok {
		return &AliasCollisionError{Alias: src.Ref(), First: other, Second: src}
	}
	f.sources[src] = true
	f.byAlias[src.Ref()] = src
	f.order = append(f.order, src)
	return nil
}

// Visible reports whether src may be referred to from the current frame.
func (s *Scope) Visible(src *Source) bool {
	for i := range s.frames {
		if s.frames[i].sources[src] {
			return true
		}
	}
	return false
}

// Sources lists everything in scope, innermost frame last, in the order each
// was added. It is used to tell an author what the query does select from.
func (s *Scope) Sources() []*Source {
	var out []*Source
	for i := range s.frames {
		out = append(out, s.frames[i].order...)
	}
	return out
}

// ScopeError reports a column belonging to a table the statement does not
// select from.
type ScopeError struct {
	// Column is the column's own name.
	Column string
	// Source is the occurrence the column belongs to.
	Source *Source
	// Visible is what the statement does select from.
	Visible []*Source
}

func (e *ScopeError) Error() string {
	var b strings.Builder
	if e.Column == "" {
		// A whole source can be out of scope without any one column being
		// named: a join condition is checked against the sources it depends
		// on before it is written, which is what makes the sequential rule
		// enforceable at all.
		fmt.Fprintf(&b, "scope error: %s is not available at this point in the query", e.Source)
	} else {
		fmt.Fprintf(&b, "scope error: column %q.%q is not available in this query", ref(e.Source), e.Column)
	}
	if len(e.Visible) == 0 {
		return b.String()
	}
	b.WriteString("\n\nthe query selects from:")
	sameName := false
	for _, s := range e.Visible {
		b.WriteString("\n  ")
		b.WriteString(s.String())
		if s.Ref() == ref(e.Source) {
			sameName = true
		}
	}
	// The confusing case is a name that is in the list: the column belongs to
	// a different occurrence of the same table, and the message would
	// otherwise look as though it were contradicting itself.
	if sameName {
		b.WriteString("\n\nthat is a different occurrence of the same table:" +
			" a column belongs to the occurrence it was built from, not to every table of that name")
	}
	return b.String()
}

func ref(s *Source) string {
	if s == nil {
		return "?"
	}
	return s.Ref()
}

// ColumnError reports a column a row source does not provide.
//
// It can only be raised for a source whose column list this package knows: a
// derived table or a CTE reference, both of which were defined by a statement
// with a select list. A table's columns come from the catalog, and the
// generator proved every descriptor against it before a query could name one.
type ColumnError struct {
	Column string
	Source *Source
}

func (e *ColumnError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has no column %q", e.Source, e.Column)
	if len(e.Source.outputs) > 0 {
		b.WriteString("; it provides:")
		for _, o := range e.Source.outputs {
			b.WriteString("\n  ")
			b.WriteString(o)
		}
	}
	return b.String()
}

// AliasCollisionError reports two occurrences claiming one name in one frame.
type AliasCollisionError struct {
	Alias         string
	First, Second *Source
}

func (e *AliasCollisionError) Error() string {
	if e.First == e.Second {
		return fmt.Sprintf("alias collision: %s is introduced twice in one query;"+
			" a second occurrence of a source needs its own alias, which As returns", e.First)
	}
	return fmt.Sprintf("alias collision: %q names both %s and %s in one query", e.Alias, e.First, e.Second)
}

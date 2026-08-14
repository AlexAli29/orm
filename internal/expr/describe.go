package expr

import (
	"fmt"
	"strings"
)

// Describing a statement without exposing it.
//
// M14's diagnostics need to say what a query is made of: which sources it
// reads, how they are joined, what it filters on, whether it locks. All of that
// is already in the tree, and none of it should be reachable from outside this
// package — an AST a caller could hold is an AST this package could no longer
// change, and one it could build is a way around scope and nullability checking.
//
// So the boundary is a description rather than the thing described. Describe
// walks the statement once and returns flat, immutable value types holding
// names, operators and counts. There are no nodes in it, no sources, and
// nowhere for a bind value to appear: the fields are strings that came from
// identifiers this package wrote, and integers it counted. What crosses the
// boundary cannot be turned back into a statement, which is the property that
// makes it safe to hand to a report, a log or a test.

// SourceInfo describes one row source.
type SourceInfo struct {
	Kind          SourceKind
	Schema, Table string
	Alias         string
	Name          string
	Recursive     bool
	Lateral       bool
}

// JoinInfo describes one join.
type JoinInfo struct {
	Kind         string
	Source       SourceInfo
	HasCondition bool
}

// OrderInfo describes one ORDER BY term.
type OrderInfo struct {
	Expr string
	Desc bool
}

// LockInfo describes a locking clause.
type LockInfo struct {
	Strength string
	Wait     string
	Of       []string
}

// SelectInfo is a flat description of a SELECT statement.
//
// Every field is derived by walking the tree once. Nothing in it is a node, and
// nothing in it is a value: FilterColumns holds column names the writer would
// have emitted, and the counts are counts.
type SelectInfo struct {
	Root       SourceInfo
	Joins      []JoinInfo
	CTEs       []SourceInfo
	Derived    []SourceInfo
	Correlated int

	FilterColumns []string
	FilterCount   int
	Projected     int
	GroupBy       []string
	OrderBy       []OrderInfo

	HasLimit  bool
	Limit     int
	HasOffset bool
	Distinct  bool

	Aggregates bool
	Windows    bool

	Lock *LockInfo
}

// Describe walks a select statement and returns a flat description of it.
func Describe(s *Select) SelectInfo {
	var info SelectInfo
	if s == nil {
		return info
	}

	if s.From != nil {
		info.Root = describeSource(s.From)
		if s.From.kind == SourceDerived {
			info.Derived = append(info.Derived, info.Root)
		}
	}
	for _, w := range s.With {
		info.CTEs = append(info.CTEs, describeSource(w))
	}
	for _, j := range s.Joins {
		ji := JoinInfo{
			Kind:         joinSQL[j.Kind],
			Source:       describeSource(j.Source),
			HasCondition: j.On != nil,
		}
		ji.Source.Lateral = j.Lateral
		info.Joins = append(info.Joins, ji)
		if j.Source != nil && j.Source.kind == SourceDerived {
			info.Derived = append(info.Derived, ji.Source)
		}
	}

	// A CROSS JOIN has no condition by construction, and a join written with an
	// empty ON is the same thing however it was spelled.
	for i := range info.Joins {
		if info.Joins[i].Kind == joinSQL[JoinCross] {
			info.Joins[i].HasCondition = false
		}
	}

	if s.Where != nil {
		info.FilterCount = countConjuncts(s.Where)
		info.FilterColumns = filterColumns(s.Where)
	}
	if s.Having != nil {
		info.FilterCount += countConjuncts(s.Having)
	}

	info.Projected = len(s.Items)
	for _, g := range s.GroupBy {
		info.GroupBy = append(info.GroupBy, renderRef(g))
	}
	for _, o := range s.OrderBy {
		info.OrderBy = append(info.OrderBy, OrderInfo{Expr: renderRef(o.node()), Desc: o.Desc})
	}

	if s.Limit != nil {
		info.HasLimit, info.Limit = true, *s.Limit
	}
	info.HasOffset = s.Offset != nil
	info.Distinct = s.Distinct || len(s.DistinctOn) > 0

	// Aggregates and windows are read from the tree rather than from a flag,
	// because either can appear anywhere an expression can.
	for _, it := range s.Items {
		if IsAggregate(it.Node) {
			info.Aggregates = true
		}
		if IsWindow(it.Node) {
			info.Windows = true
		}
	}
	if s.Having != nil {
		info.Aggregates = true
	}

	info.Correlated = countCorrelated(s)

	if s.Lock.Strength != LockNone {
		li := &LockInfo{Strength: s.Lock.Strength.String(), Wait: strings.TrimSpace(lockWaitSQL[s.Lock.Wait])}
		for _, src := range s.Lock.Of {
			li.Of = append(li.Of, sourceName(src))
		}
		info.Lock = li
	} else if s.ForUpdate {
		info.Lock = &LockInfo{Strength: "FOR UPDATE"}
	}
	return info
}

func describeSource(s *Source) SourceInfo {
	if s == nil {
		return SourceInfo{}
	}
	return SourceInfo{
		Kind:      s.kind,
		Schema:    s.schema,
		Table:     s.table,
		Alias:     s.alias,
		Name:      s.cTEName,
		Recursive: s.recursive,
	}
}

// sourceName is how a person would refer to the source, which is its alias when
// it has one and its own name otherwise.
func sourceName(s *Source) string {
	if s == nil {
		return ""
	}
	if s.alias != "" {
		return s.alias
	}
	if s.table != "" {
		return s.table
	}
	return s.cTEName
}

// countConjuncts counts the top-level conditions a predicate carries, so that
// "three filters" means three things a reader would recognise as filters rather
// than the size of the tree.
func countConjuncts(n Node) int {
	g, ok := n.(Group)
	if !ok || g.Op != OpAnd {
		return 1
	}
	total := 0
	for _, it := range g.Items {
		total += countConjuncts(it)
	}
	return total
}

// filterColumns names the columns a predicate mentions, deduplicated and in the
// order first seen.
//
// It reads columns and nothing else. A bind value is an Arg and an Arg has no
// name, so there is nothing here that could carry one out.
func filterColumns(n Node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(Node)
	walk = func(x Node) {
		if x == nil {
			return
		}
		if c, ok := x.(Column); ok {
			name := columnRef(c)
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		children(x, walk)
	}
	walk(n)
	return out
}

// columnRef renders a column the way the statement refers to it.
func columnRef(c Column) string {
	q := sourceName(c.Source)
	if q == "" {
		return c.Name
	}
	return q + "." + c.Name
}

// renderRef names an expression for a report: a column by name, anything else
// by what kind of expression it is.
//
// Rendering the expression itself would mean a second writer, and a second
// writer is the thing this milestone is not allowed to have. Naming the kind is
// enough for a reader to recognise the term, and cannot leak a value.
func renderRef(n Node) string {
	switch x := n.(type) {
	case nil:
		return ""
	case Column:
		return columnRef(x)
	case Cast:
		return renderRef(x.X)
	case Call:
		return x.Func + "(...)"
	case Aggregate:
		return strings.ToLower(x.Func) + "(...)"
	case Infix:
		return "expression " + x.Op
	case Arith:
		return "arithmetic"
	case Case:
		return "CASE"
	case Arg:
		return "parameter"
	default:
		return fmt.Sprintf("%T expression", n)
	}
}

// countCorrelated counts subqueries that name a source introduced outside
// themselves, which is the shape that may be evaluated per outer row.
func countCorrelated(s *Select) int {
	outer := map[*Source]bool{}
	if s.From != nil {
		outer[s.From] = true
	}
	for _, j := range s.Joins {
		outer[j.Source] = true
	}
	for _, w := range s.With {
		outer[w] = true
	}

	count := 0
	check := func(n Node) {
		if n == nil {
			return
		}
		var walk func(Node)
		walk = func(x Node) {
			if sub := subqueryOf(x); sub != nil {
				free := false
				sub.free(func(src *Source) {
					if outer[src] {
						free = true
					}
				})
				if free {
					count++
				}
			}
			children(x, walk)
		}
		walk(n)
	}
	check(s.Where)
	check(s.Having)
	for _, it := range s.Items {
		check(it.Node)
	}
	return count
}

// DescribeUpdate flattens an UPDATE the same way Describe flattens a SELECT.
func DescribeUpdate(u *Update) SelectInfo {
	var info SelectInfo
	if u == nil {
		return info
	}
	info.Root = describeSource(u.Table)
	if u.Where != nil {
		info.FilterCount = countConjuncts(u.Where)
		info.FilterColumns = filterColumns(u.Where)
	}
	info.Projected = len(u.Set)
	return info
}

// DescribeDelete flattens a DELETE.
func DescribeDelete(d *Delete) SelectInfo {
	var info SelectInfo
	if d == nil {
		return info
	}
	info.Root = describeSource(d.From)
	if d.Where != nil {
		info.FilterCount = countConjuncts(d.Where)
		info.FilterColumns = filterColumns(d.Where)
	}
	return info
}

// DescribeInsert flattens an INSERT.
func DescribeInsert(i *Insert) SelectInfo {
	var info SelectInfo
	if i == nil {
		return info
	}
	info.Root = describeSource(i.Into)
	info.Projected = len(i.Columns)
	return info
}

// ReturningCount is how many expressions a write hands back, which decides
// whether the operation buffers.
func ReturningCount(returning []Column, items []SelectItem) int {
	return len(returning) + len(items)
}

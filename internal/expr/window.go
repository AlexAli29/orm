package expr

import (
	"fmt"
	"strconv"
)

// Window functions.
//
// A window function is an aggregate that does not group. It sees a set of rows
// — the window — computes over them, and returns one value for the row it was
// called on, so the statement's cardinality is untouched. That is why it is the
// same node as an aggregate here with one field added: the difference between
// count(*) and count(*) OVER () is the OVER clause and nothing else, and giving
// them separate nodes would mean two writers, two dependency walks and two
// places for the FILTER clause to be handled differently.
//
// What the added field changes is where the node may appear. A grouping
// aggregate belongs in a select list or a HAVING clause; a window function
// belongs in a select list or an ORDER BY, and PostgreSQL rejects it anywhere
// else — including in a HAVING clause, because windows are computed after
// grouping has already happened. Filtering on one means wrapping the statement,
// which is what a derived table is for.

// WindowSpec is the window a function computes over.
//
// The zero specification is OVER (), which is the whole result set as one
// unordered partition — legal, occasionally what is wanted, and the reason an
// empty spec is not an error.
type WindowSpec struct {
	PartitionBy []Node
	// OrderBy is the window's own ordering. It is not the statement's: it
	// decides which rows come before this one inside the partition, and a
	// query can order its output by something else entirely.
	OrderBy []Order
	Frame   *Frame
}

// walk visits every expression the specification holds, in render order.
func (s *WindowSpec) walk(visit func(Node)) {
	if s == nil {
		return
	}
	for _, p := range s.PartitionBy {
		visit(p)
	}
	for _, o := range s.OrderBy {
		visit(o.node())
	}
}

// FrameMode is how a frame counts.
type FrameMode uint8

// The frame modes PostgreSQL offers. ROWS counts rows, RANGE counts values that
// order equally with the current row, and GROUPS counts peer groups.
const (
	frameModeInvalid FrameMode = iota
	FrameRows
	FrameRange
	FrameGroups
)

var frameModeSQL = map[FrameMode]string{
	FrameRows:   "ROWS",
	FrameRange:  "RANGE",
	FrameGroups: "GROUPS",
}

// BoundKind is where a frame edge sits relative to the current row.
type BoundKind uint8

// The frame bounds, in the order PostgreSQL's grammar allows them to appear:
// a frame may not start after it ends.
const (
	boundInvalid BoundKind = iota
	UnboundedPreceding
	Preceding
	CurrentRow
	Following
	UnboundedFollowing
)

// Bound is one edge of a frame.
type Bound struct {
	Kind BoundKind
	// Offset is how many rows, values or groups the edge is from the current
	// row, for Preceding and Following.
	//
	// It is an integer rather than an expression because PostgreSQL's grammar
	// takes an expression here but forbids one that refers to a column, and
	// the useful remainder is a constant. Rendering a checked non-negative
	// integer is therefore both safe and complete: there is no string from a
	// caller anywhere near this position.
	Offset int
}

// Frame is the subset of the partition a function computes over.
type Frame struct {
	Mode       FrameMode
	Start, End Bound
}

// validate refuses a frame PostgreSQL's grammar would.
func (f *Frame) validate() error {
	if _, ok := frameModeSQL[f.Mode]; !ok {
		return fmt.Errorf("a window frame has no mode")
	}
	for _, b := range []Bound{f.Start, f.End} {
		switch b.Kind {
		case UnboundedPreceding, CurrentRow, UnboundedFollowing:
		case Preceding, Following:
			if b.Offset < 0 {
				return fmt.Errorf("a window frame offset cannot be negative, and this one is %d", b.Offset)
			}
		default:
			return fmt.Errorf("a window frame bound has no kind")
		}
	}
	switch {
	case f.Start.Kind == UnboundedFollowing:
		return fmt.Errorf("a window frame cannot start at UNBOUNDED FOLLOWING")
	case f.End.Kind == UnboundedPreceding:
		return fmt.Errorf("a window frame cannot end at UNBOUNDED PRECEDING")
	case f.Start.Kind > f.End.Kind:
		return fmt.Errorf("a window frame cannot start after it ends")
	}
	return nil
}

func (w *writer) writeWindow(s *WindowSpec) {
	w.b.WriteString(" OVER (")
	if s != nil {
		if len(s.PartitionBy) > 0 {
			w.b.WriteString("PARTITION BY ")
			for i, p := range s.PartitionBy {
				if i > 0 {
					w.b.WriteString(", ")
				}
				w.node(p, false)
			}
		}
		if len(s.OrderBy) > 0 {
			if len(s.PartitionBy) > 0 {
				w.b.WriteByte(' ')
			}
			w.b.WriteString("ORDER BY ")
			w.writeOrderTerms(s.OrderBy)
		}
		if s.Frame != nil {
			if len(s.PartitionBy) > 0 || len(s.OrderBy) > 0 {
				w.b.WriteByte(' ')
			}
			w.writeFrame(s.Frame)
		}
	}
	w.b.WriteByte(')')
}

func (w *writer) writeFrame(f *Frame) {
	if err := f.validate(); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteString(frameModeSQL[f.Mode])
	w.b.WriteString(" BETWEEN ")
	w.writeBound(f.Start)
	w.b.WriteString(" AND ")
	w.writeBound(f.End)
}

func (w *writer) writeBound(b Bound) {
	switch b.Kind {
	case UnboundedPreceding:
		w.b.WriteString("UNBOUNDED PRECEDING")
	case CurrentRow:
		w.b.WriteString("CURRENT ROW")
	case UnboundedFollowing:
		w.b.WriteString("UNBOUNDED FOLLOWING")
	case Preceding:
		w.b.WriteString(strconv.Itoa(b.Offset))
		w.b.WriteString(" PRECEDING")
	case Following:
		w.b.WriteString(strconv.Itoa(b.Offset))
		w.b.WriteString(" FOLLOWING")
	}
}

// IsWindow reports whether a node is a window function call.
func IsWindow(n Node) bool {
	a, ok := n.(Aggregate)
	return ok && a.Over != nil
}

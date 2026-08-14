package plan

import (
	"fmt"
	"strings"
)

// Walking a plan.
//
// A plan is a tree and every question about one is a traversal, so the
// traversals live here rather than in every caller. They hand out copies: a
// plan is a record of what the server decided, and something that edited it
// while reading would be describing a plan that never existed.

// Walk calls fn for every node, depth-first, root first.
//
// The node is passed by value. Modifying it changes nothing, which is the
// intent: this is a reader.
func (p *Plan) Walk(fn func(Node)) {
	if p == nil {
		return
	}
	p.Root.Walk(fn)
}

// Walk calls fn for this node and every node beneath it, depth-first.
func (n Node) Walk(fn func(Node)) {
	fn(n)
	for _, c := range n.Plans {
		c.Walk(fn)
	}
}

// Nodes returns every node, depth-first.
func (p *Plan) Nodes() []Node {
	var out []Node
	p.Walk(func(n Node) { out = append(out, n) })
	return out
}

// Find returns every node fn accepts, depth-first.
func (p *Plan) Find(fn func(Node) bool) []Node {
	var out []Node
	p.Walk(func(n Node) {
		if fn(n) {
			out = append(out, n)
		}
	})
	return out
}

// OfType returns every node of the given type.
func (p *Plan) OfType(t NodeType) []Node {
	return p.Find(func(n Node) bool { return n.Type == t })
}

// Node returns the node with the given ID, and whether there is one.
//
// The IDs are assigned during parsing, so this is how a diagnostic that named a
// node gets it back.
func (p *Plan) Node(id int) (Node, bool) {
	var found Node
	var ok bool
	p.Walk(func(n Node) {
		if n.ID == id && !ok {
			found, ok = n, true
		}
	})
	return found, ok
}

// Depth is how many levels the tree has, counting the root as one.
func (p *Plan) Depth() int {
	if p == nil {
		return 0
	}
	return p.Root.depth()
}

func (n Node) depth() int {
	deepest := 0
	for _, c := range n.Plans {
		if d := c.depth(); d > deepest {
			deepest = d
		}
	}
	return deepest + 1
}

// TotalRows is how many rows this node produced in all of its loops.
//
// PostgreSQL reports "Actual Rows" as the average per loop, so a node inside a
// nested loop that ran a thousand times and produced one row each time reports
// one. Multiplying by the loop count is how the total is obtained, and getting
// that wrong is the most common way to misread an analysed plan.
//
// It returns false when the plan was not analysed.
func (n Node) TotalRows() (float64, bool) {
	if n.ActualRows == nil {
		return 0, false
	}
	loops := 1.0
	if n.ActualLoops != nil {
		loops = *n.ActualLoops
	}
	return *n.ActualRows * loops, true
}

// SelfTime is how long this node took excluding the nodes beneath it, in
// milliseconds.
//
// PostgreSQL's "Actual Total Time" is inclusive and per loop. This multiplies by
// the loops and subtracts the children, which is what makes "where did the time
// go" answerable. It returns false when the plan was not analysed.
func (n Node) SelfTime() (float64, bool) {
	if n.ActualTotalTime == nil {
		return 0, false
	}
	loops := 1.0
	if n.ActualLoops != nil {
		loops = *n.ActualLoops
	}
	total := *n.ActualTotalTime * loops
	for _, c := range n.Plans {
		if c.ActualTotalTime != nil {
			cl := 1.0
			if c.ActualLoops != nil {
				cl = *c.ActualLoops
			}
			total -= *c.ActualTotalTime * cl
		}
	}
	if total < 0 {
		// Rounding in the server's own numbers can make a parent look faster
		// than its children. Reporting a negative duration would be worse than
		// reporting nothing.
		total = 0
	}
	return total, true
}

// IsScan reports whether the node reads a relation directly.
func (n Node) IsScan() bool {
	switch n.Type {
	case SeqScan, IndexScan, IndexOnlyScan, BitmapHeapScan, BitmapIndexScan, TidScan:
		return true
	default:
		return strings.HasSuffix(string(n.Type), "Scan")
	}
}

// Condition returns the node's own filtering condition and which kind it is, or
// two empty strings when it has none.
//
// A node can carry several — an index scan with both an Index Cond and a Filter
// — so this returns the most specific one. [Node.Conditions] returns all of
// them.
func (n Node) Condition() (kind, cond string) {
	for _, c := range n.Conditions() {
		return c.Kind, c.Cond
	}
	return "", ""
}

// Cond is one condition a node carries, with the kind PostgreSQL called it.
type Cond struct {
	// Kind is which condition this is, as PostgreSQL names it: "Filter",
	// "Index Cond", "Hash Cond" and so on.
	Kind string
	// Cond is the expression, verbatim from the server.
	//
	// It is not value-free: PostgreSQL writes the constants it planned with
	// into the text. That is why [Plan.Redacted] clears it and why the report
	// rendering withholds it by default.
	Cond string
}

// Conditions returns every condition the node carries, most specific first.
func (n Node) Conditions() []Cond {
	var out []Cond
	for _, c := range []Cond{
		{"Index Cond", n.IndexCond},
		{"Hash Cond", n.HashCond},
		{"Merge Cond", n.MergeCond},
		{"TID Cond", n.TidCond},
		{"Recheck Cond", n.RecheckCond},
		{"Join Filter", n.JoinFilter},
		{"Filter", n.Filter},
		{"One-Time Filter", n.OneTimeFilter},
	} {
		if c.Cond != "" {
			out = append(out, c)
		}
	}
	return out
}

// Target names what the node reads, as a person would refer to it: the relation
// with its alias when they differ, or the index, or the CTE.
func (n Node) Target() string {
	switch {
	case n.RelationName != "" && n.Alias != "" && n.Alias != n.RelationName:
		return n.RelationName + " " + n.Alias
	case n.RelationName != "":
		return n.RelationName
	case n.IndexName != "":
		return n.IndexName
	case n.CTEName != "":
		return n.CTEName
	case n.FunctionName != "":
		return n.FunctionName
	case n.SubplanName != "":
		return n.SubplanName
	default:
		return ""
	}
}

// Summary is what can be worked out about a plan by looking at it.
//
// Everything here is derived rather than reported. It is a separate type from
// [Plan] for exactly that reason: a caller reading Summary knows it is looking
// at arithmetic this package did, and a caller reading Plan knows it is looking
// at what the server said.
type Summary struct {
	// Nodes is how many nodes the plan has.
	Nodes int
	// Depth is how many levels it has.
	Depth int

	// Relations are the tables the plan reads, sorted and deduplicated.
	Relations []string
	// Indexes are the indexes it uses, sorted and deduplicated.
	Indexes []string
	// ScanTypes counts the scan nodes by type.
	ScanTypes map[NodeType]int

	// SeqScans are the sequential scans, which is the question asked most often.
	SeqScans []Node

	// Parallel is true when any node is parallel-aware.
	Parallel bool
	// WorkersPlanned and WorkersLaunched are the totals across Gather nodes.
	WorkersPlanned  int64
	WorkersLaunched int64

	// Spills are the nodes that used disk: a sort with an external method, a
	// hash with more than one batch, anything reporting temp blocks.
	Spills []Node

	// SlowestSelf is the node that spent the most time on its own work, present
	// only for an analysed plan.
	SlowestSelf *Node
	// SelfTime is that node's own time in milliseconds.
	SelfTime float64
}

// Summarize derives a summary of the plan.
//
// It reads the plan and computes; it asks the server nothing and decides
// nothing. The judgements live in the diagnostics package.
func (p *Plan) Summarize() Summary {
	s := Summary{ScanTypes: map[NodeType]int{}}
	if p == nil {
		return s
	}
	s.Depth = p.Depth()

	relations := map[string]bool{}
	indexes := map[string]bool{}
	var slowest *Node
	slowestTime := -1.0

	p.Walk(func(n Node) {
		s.Nodes++
		if n.ParallelAware {
			s.Parallel = true
		}
		if n.WorkersPlanned != nil {
			s.WorkersPlanned += *n.WorkersPlanned
		}
		if n.WorkersLaunched != nil {
			s.WorkersLaunched += *n.WorkersLaunched
		}
		if n.RelationName != "" {
			relations[n.RelationName] = true
		}
		if n.IndexName != "" {
			indexes[n.IndexName] = true
		}
		if n.IsScan() {
			s.ScanTypes[n.Type]++
		}
		if n.Type == SeqScan {
			s.SeqScans = append(s.SeqScans, n)
		}
		if n.spilled() {
			s.Spills = append(s.Spills, n)
		}
		if t, ok := n.SelfTime(); ok && t > slowestTime {
			copied := n
			slowest, slowestTime = &copied, t
		}
	})

	s.Relations = sortedKeys(relations)
	s.Indexes = sortedKeys(indexes)
	if slowest != nil {
		s.SlowestSelf = slowest
		s.SelfTime = slowestTime
	}
	return s
}

// spilled reports whether the node used disk rather than memory.
func (n Node) spilled() bool {
	if n.SortSpaceType == "Disk" {
		return true
	}
	if n.HashBatches != nil && *n.HashBatches > 1 {
		return true
	}
	if n.HashAggBatches != nil && *n.HashAggBatches > 1 {
		return true
	}
	if n.Buffers != nil {
		if n.Buffers.TempWritten != nil && *n.Buffers.TempWritten > 0 {
			return true
		}
		if n.Buffers.TempRead != nil && *n.Buffers.TempRead > 0 {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort: these lists are a handful of names, and a stable order is
	// all that is wanted.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// String renders the plan the way EXPLAIN's text format does, near enough to
// read.
//
// It is a rendering of the typed model rather than the server's text output, and
// it says so by existing here rather than being called the plan: nothing parses
// it back, and it is not promised to match PostgreSQL's own formatting.
func (p *Plan) String() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	p.Root.render(&b, 0)
	if p.PlanningTime != nil {
		fmtLine(&b, 0, "Planning Time: %.3f ms", *p.PlanningTime)
	}
	if p.ExecutionTime != nil {
		fmtLine(&b, 0, "Execution Time: %.3f ms", *p.ExecutionTime)
	}
	return b.String()
}

// fmtLine writes one indented line of the rendered plan.
func fmtLine(b *strings.Builder, depth int, format string, args ...any) {
	b.WriteString(strings.Repeat("  ", depth))
	fmt.Fprintf(b, format, args...)
	b.WriteByte('\n')
}

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func (n Node) render(b *strings.Builder, depth int) {
	line := string(n.Type)
	if t := n.Target(); t != "" {
		line += " on " + t
	}
	if n.TotalCost != nil && n.PlanRows != nil {
		line += fmtSprintf("  (cost=%.2f rows=%.0f)", *n.TotalCost, *n.PlanRows)
	}
	if n.ActualRows != nil {
		loops := 1.0
		if n.ActualLoops != nil {
			loops = *n.ActualLoops
		}
		line += fmtSprintf(" (actual rows=%.0f loops=%.0f)", *n.ActualRows, loops)
	}
	fmtLine(b, depth, "%s", line)
	for _, c := range n.Conditions() {
		fmtLine(b, depth+1, "%s: %s", c.Kind, c.Cond)
	}
	for _, c := range n.Plans {
		c.render(b, depth+1)
	}
}

// Redacted returns a copy of the plan with the free-form expression text
// removed from every node.
//
// PostgreSQL writes the constants it planned with into the strings it reports —
// a condition on a parameterised statement reads
// (email = 'someone@example.com') even though the statement carried a
// placeholder — so those strings are the one part of a plan that can carry a
// caller's data. Everything else is structure and numbers.
//
// This removes exactly the free-form fields and keeps the rest, so a redacted
// plan still says which node types ran, over which relations, through which
// indexes, with what costs, rows, loops, buffers and timings. It is what a plan
// looks like when it has to be safe to serialise.
//
// The fields cleared are the conditions — Filter, Index Cond, Recheck Cond,
// Join Filter, Hash Cond, Merge Cond, TID Cond and One-Time Filter — together
// with Output, Sort Key, Presorted Key and Group Key, each of which is an
// expression list that can contain a literal.
//
// The original is untouched: a redacted plan is a new tree.
func (p *Plan) Redacted() *Plan {
	if p == nil {
		return nil
	}
	out := *p
	out.Root = p.Root.redacted()
	// The raw JSON is the server's answer verbatim and is the thing being
	// protected, so a redacted plan does not carry it.
	out.raw = nil
	return &out
}

// Redacted returns a copy of the node and its children with the free-form
// expression text removed. See [Plan.Redacted].
func (n Node) Redacted() Node { return n.redacted() }

func (n Node) redacted() Node {
	n.Filter = ""
	n.IndexCond = ""
	n.RecheckCond = ""
	n.JoinFilter = ""
	n.HashCond = ""
	n.MergeCond = ""
	n.TidCond = ""
	n.OneTimeFilter = ""
	n.Output = nil
	n.SortKey = nil
	n.PresortedKey = nil
	n.GroupKey = nil
	// Extra holds the fields this package has not modelled, which by definition
	// includes any future condition-bearing one.
	n.Extra = nil

	if len(n.Plans) > 0 {
		kids := make([]Node, len(n.Plans))
		for i, c := range n.Plans {
			kids[i] = c.redacted()
		}
		n.Plans = kids
	}
	return n
}

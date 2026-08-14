// Package plan is PostgreSQL's own query plan, typed.
//
// EXPLAIN has two output formats and only one of them is a contract. The text
// format is written for people and rearranged whenever the developers think of
// something clearer; the JSON format is documented, versioned by the server's
// own release notes, and machine-readable. This package reads the JSON, and
// there is no text parser anywhere in it.
//
// Three decisions run through the whole model, and each is a decision about
// being wrong later rather than about being tidy now.
//
// Nothing here is a closed set. [NodeType] is a string, not an enum: PostgreSQL
// 18 will have a node type PostgreSQL 17 does not, and a package that could not
// parse it would stop working on upgrade. Unknown node types are inspectable
// like every other.
//
// Optional fields are pointers. A plan node has no "Actual Rows" unless the
// statement was run with ANALYZE, and a plan with zero actual rows is a
// different fact from a plan that was never analysed. Zero cannot mean both.
//
// Fields nobody has modelled are kept rather than dropped. Every node carries
// its unrecognised JSON in [Node.Extra], so a server field this package has not
// caught up with is still readable by whoever needs it.
//
// The package deliberately depends on nothing but the standard library. It is
// the plan, not the ORM's opinion about the plan — the diagnostics live
// elsewhere, and anything this package derives says so.
package plan

import (
	"encoding/json"
	"fmt"
)

// Plan is one statement's plan, as EXPLAIN (FORMAT JSON) returns it.
//
// EXPLAIN returns an array with one element per statement, and every statement
// this ORM builds is one statement, so [Parse] returns the first and reports it
// if there are more.
type Plan struct {
	// Root is the outermost plan node.
	Root Node

	// PlanningTime is milliseconds spent planning, present when the server
	// reported it. EXPLAIN without ANALYZE still reports it on every supported
	// version.
	PlanningTime *float64
	// ExecutionTime is milliseconds spent executing, present only with ANALYZE.
	ExecutionTime *float64

	// Analyzed records whether the statement was actually run. It is derived
	// from the presence of execution timing rather than from what the caller
	// asked for, so a plan that arrived from somewhere else still knows.
	Analyzed bool

	// Settings holds the non-default planner settings the server reported, when
	// SETTINGS was requested. They are context for a plan, not something to
	// change.
	Settings map[string]string
	// Triggers fired during an analysed write.
	Triggers []Trigger
	// JIT is the just-in-time compilation summary, when the server reported one.
	JIT *JIT

	// QueryIdentifier is PostgreSQL's own query id, when compute_query_id is on.
	// It is not this package's fingerprint and the two are unrelated.
	QueryIdentifier *int64

	// Extra holds the top-level JSON this package does not model, so that a
	// field a newer server adds is still reachable.
	Extra map[string]json.RawMessage

	// raw is the JSON the plan was parsed from, kept so that a caller who needs
	// something the model does not have can read it without asking the server
	// twice.
	raw json.RawMessage
}

// JSON returns the plan's original JSON.
//
// It is the escape hatch for a field this package has not modelled, and it is
// the server's bytes rather than a re-encoding of the typed model.
func (p *Plan) JSON() json.RawMessage { return append(json.RawMessage(nil), p.raw...) }

// Trigger is one trigger's cost, reported for an analysed write.
type Trigger struct {
	// Name is the trigger, Relation the table it fired on.
	Name     string
	Relation string
	// Time is the total milliseconds spent in it across Calls invocations.
	// Trigger time is not attributed to any plan node, so a statement whose
	// nodes account for less than its total often spent the difference here.
	Time  float64
	Calls int64
}

// JIT is the just-in-time compilation summary.
//
// It is present only when the server compiled something, which depends on
// configuration and on the plan's cost, so nothing here requires it.
type JIT struct {
	// Functions is how many were compiled, and Options records which of
	// PostgreSQL's JIT stages were enabled.
	Functions *int64
	Options   map[string]any
	// Milliseconds per stage, and their total. JIT is decided from the
	// planner's cost estimate, so a badly estimated statement can spend longer
	// compiling than it would have spent interpreting — which is visible here
	// and nowhere else.
	Inlining     *float64
	Optimization *float64
	Emission     *float64
	Generation   *float64
	Total        *float64
}

// NodeType is a plan node's kind, as PostgreSQL names it.
//
// It is a string rather than an enumeration on purpose: a closed set would make
// a node type from a newer server unparseable, which is the opposite of what a
// plan reader is for. The constants below are the common ones, offered for
// comparison rather than as an exhaustive list.
type NodeType string

// The node types PostgreSQL produces most often. A node type not listed here is
// not an error and not less usable.
const (
	SeqScan         NodeType = "Seq Scan"
	IndexScan       NodeType = "Index Scan"
	IndexOnlyScan   NodeType = "Index Only Scan"
	BitmapIndexScan NodeType = "Bitmap Index Scan"
	BitmapHeapScan  NodeType = "Bitmap Heap Scan"
	TidScan         NodeType = "Tid Scan"
	SubqueryScan    NodeType = "Subquery Scan"
	FunctionScan    NodeType = "Function Scan"
	ValuesScan      NodeType = "Values Scan"
	CTEScan         NodeType = "CTE Scan"
	WorkTableScan   NodeType = "WorkTable Scan"
	NamedTuplestore NodeType = "Named Tuplestore Scan"
	NestedLoop      NodeType = "Nested Loop"
	MergeJoin       NodeType = "Merge Join"
	HashJoin        NodeType = "Hash Join"
	Hash            NodeType = "Hash"
	Materialize     NodeType = "Materialize"
	Memoize         NodeType = "Memoize"
	Sort            NodeType = "Sort"
	IncrementalSort NodeType = "Incremental Sort"
	Group           NodeType = "Group"
	Aggregate       NodeType = "Aggregate"
	WindowAgg       NodeType = "WindowAgg"
	Unique          NodeType = "Unique"
	SetOp           NodeType = "SetOp"
	LockRows        NodeType = "LockRows"
	Limit           NodeType = "Limit"
	Append          NodeType = "Append"
	MergeAppend     NodeType = "Merge Append"
	RecursiveUnion  NodeType = "Recursive Union"
	Result          NodeType = "Result"
	ProjectSet      NodeType = "ProjectSet"
	ModifyTable     NodeType = "ModifyTable"
	Gather          NodeType = "Gather"
	GatherMerge     NodeType = "Gather Merge"
)

// Node is one node of the plan tree.
//
// Every field PostgreSQL reports conditionally is a pointer or a slice, so that
// "the server did not say" and "the server said zero" stay apart. A node
// scanning no rows and a node that was never run are different facts and a
// diagnostic that confused them would be wrong about which.
type Node struct {
	// Type is what PostgreSQL called this node — "Seq Scan", "Hash Join",
	// "Gather". It is a string rather than an enumeration so that a node type
	// added by a future PostgreSQL parses instead of failing.
	Type NodeType

	// ID is assigned during parsing, breadth-first from zero, so that a
	// diagnostic can name the node it is about. It is this package's number and
	// not the server's.
	ID int
	// Path is the node's position in the tree, as node types from the root.
	// It is derived, and it exists because "Seq Scan on posts" is ambiguous in a
	// plan with two of them.
	Path []NodeType

	// Structure.
	//
	// ParentRelationship is how this node feeds its parent — "Outer", "Inner",
	// "Member", "InitPlan", "SubPlan" — which is what tells the two sides of a
	// join apart. SubplanName names an InitPlan or SubPlan.
	ParentRelationship string
	SubplanName        string
	// ParallelAware reports that the node itself participates in parallelism
	// rather than merely sitting above a Gather. AsyncCapable reports that it
	// can be run asynchronously, which foreign scans use.
	ParallelAware bool
	AsyncCapable  bool
	// Plans are this node's children, in the order the server reported them.
	Plans []Node

	// What is being read.
	RelationName  string
	Schema        string
	Alias         string
	IndexName     string
	ScanDirection string
	CTEName       string
	FunctionName  string
	TupleStore    string

	// How rows are combined.
	JoinType    string
	InnerUnique *bool
	Strategy    string
	PartialMode string
	Operation   string

	// The planner's estimates. Costs are always present without COSTS OFF.
	StartupCost *float64
	TotalCost   *float64
	PlanRows    *float64
	PlanWidth   *int64

	// What actually happened, present only with ANALYZE.
	ActualStartupTime *float64
	ActualTotalTime   *float64
	ActualRows        *float64
	ActualLoops       *float64

	// Conditions and filters, as the server rendered them. They are strings
	// because they are the server's expression printer's output, not this
	// package's AST.
	Filter                    string
	IndexCond                 string
	RecheckCond               string
	JoinFilter                string
	HashCond                  string
	MergeCond                 string
	TidCond                   string
	OneTimeFilter             string
	RowsRemovedByFilter       *float64
	RowsRemovedByJoin         *float64
	RowsRemovedByRecheck      *float64
	RowsRemovedByIndexRecheck *float64
	HeapFetches               *float64
	ExactHeapBlocks           *float64
	LossyHeapBlocks           *float64

	// Ordering and grouping.
	SortKey         []string
	PresortedKey    []string
	SortMethod      string
	SortSpaceUsed   *int64
	SortSpaceType   string
	GroupKey        []string
	HashAggBatches  *int64
	PeakMemoryUsage *int64

	// Hashing.
	//
	// Batches above one mean the hash did not fit in work_mem and was spilled
	// to disk in pieces. The Original values are what the planner expected
	// before execution adjusted them, so Batches differing from
	// OriginalHashBatches is the plan saying it was surprised.
	HashBuckets         *int64
	OriginalHashBuckets *int64
	HashBatches         *int64
	OriginalHashBatches *int64

	// Parallelism.
	//
	// Fewer launched than planned means the server ran out of parallel worker
	// slots, and the statement was slower than its plan implies for a reason
	// that has nothing to do with the statement.
	WorkersPlanned  *int64
	WorkersLaunched *int64

	// Output columns, present with VERBOSE.
	Output []string

	// Buffers, present with BUFFERS.
	Buffers *Buffers
	// WAL, present with WAL on an analysed write.
	WAL *WAL

	// Extra holds this node's unrecognised JSON.
	Extra map[string]json.RawMessage
}

// Buffers is one node's buffer accounting.
//
// Every field is a pointer because the server reports the group only when
// BUFFERS was asked for, and reports the I/O timings only when track_io_timing
// is on.
type Buffers struct {
	// Shared buffers hold ordinary table and index data, and are the cache
	// every backend shares. A block counted as hit was already in memory; one
	// counted as read was not, and had to come from the operating system or the
	// disk. The ratio between them is the usual reason one node dominates a
	// plan's time while another with more rows does not.
	SharedHit  *int64
	SharedRead *int64
	// Dirtied counts blocks this statement modified for the first time since
	// the last checkpoint; written counts blocks it had to flush itself because
	// no clean buffer was available. Writes attributed to a SELECT are usually
	// this, not a mistake in reading the plan.
	SharedDirtied *int64
	SharedWritten *int64

	// Local buffers are the same measurements for temporary tables, which live
	// in the backend's own memory rather than the shared cache.
	LocalHit     *int64
	LocalRead    *int64
	LocalDirtied *int64
	LocalWritten *int64

	// Temp blocks are the spill: data written and read back during a sort or a
	// hash that did not fit in work_mem. A node with a large TempWritten did
	// the work twice, once to put the rows on disk and once to fetch them.
	TempRead    *int64
	TempWritten *int64

	// I/O timings in milliseconds, present when track_io_timing is on.
	//
	// They are absent rather than zero when it is off, which is why they are
	// pointers: a node that spent no time reading and a server that was not
	// measuring are different facts.
	SharedReadTime  *float64
	SharedWriteTime *float64
	LocalReadTime   *float64
	LocalWriteTime  *float64
	TempReadTime    *float64
	TempWriteTime   *float64
}

// WAL is one node's write-ahead-log accounting, present with WAL and ANALYZE.
// WAL is the write-ahead log a statement produced.
//
// It is what makes a write expensive beyond its own table: every byte here is
// replicated, archived and replayed on every standby.
type WAL struct {
	// Records is how many WAL records the statement wrote and Bytes their
	// total size. FPI counts full-page images: whole pages written because the
	// page had not been touched since the last checkpoint, which is why the
	// same statement costs more just after one.
	Records *int64
	FPI     *int64
	Bytes   *float64
	// Buffers is how many WAL buffers were written out during the statement.
	Buffers *int64
}

// ParseError reports JSON that is not an EXPLAIN result.
type ParseError struct {
	// Reason says what was wrong.
	Reason string
	// Err is the decoding error underneath, when there was one.
	Err error
}

// Error says which part of the plan JSON could not be read.
func (e *ParseError) Error() string {
	if e.Err != nil {
		return "plan: " + e.Reason + ": " + e.Err.Error()
	}
	return "plan: " + e.Reason
}

// Unwrap returns the underlying decoding failure, so a caller can match on it
// with errors.Is and errors.As.
func (e *ParseError) Unwrap() error { return e.Err }

// Parse reads the JSON an EXPLAIN (FORMAT JSON) produced.
//
// It is deliberately tolerant of everything except being handed something that
// is not a plan: an unknown node type parses, an unknown field is kept, a field
// this package expects and does not find is simply absent. That tolerance is
// the point — a plan reader that failed on a newer server would be a plan
// reader nobody could upgrade past.
func Parse(data []byte) (*Plan, error) {
	var wrappers []planWrapper
	if err := json.Unmarshal(data, &wrappers); err != nil {
		// A single object rather than an array is what some tools produce, and
		// accepting it costs nothing.
		var one planWrapper
		if err2 := json.Unmarshal(data, &one); err2 != nil {
			return nil, &ParseError{Reason: "this is not EXPLAIN (FORMAT JSON) output", Err: err}
		}
		wrappers = []planWrapper{one}
	}
	if len(wrappers) == 0 {
		return nil, &ParseError{Reason: "the EXPLAIN result has no plan in it"}
	}
	if len(wrappers) > 1 {
		return nil, &ParseError{Reason: fmt.Sprintf(
			"the EXPLAIN result has %d plans in it, and one statement has one", len(wrappers))}
	}
	w := wrappers[0]
	if len(w.Plan) == 0 {
		return nil, &ParseError{Reason: `the EXPLAIN result has no "Plan" key`}
	}

	root, err := parseNode(w.Plan)
	if err != nil {
		return nil, err
	}

	p := &Plan{
		Root:            root,
		PlanningTime:    w.PlanningTime,
		ExecutionTime:   w.ExecutionTime,
		Analyzed:        w.ExecutionTime != nil,
		QueryIdentifier: w.QueryIdentifier,
		raw:             append(json.RawMessage(nil), data...),
	}
	if len(w.Settings) > 0 {
		p.Settings = make(map[string]string, len(w.Settings))
		for k, v := range w.Settings {
			p.Settings[k] = stringOf(v)
		}
	}
	for _, t := range w.Triggers {
		p.Triggers = append(p.Triggers, Trigger{
			Name: t.TriggerName, Relation: t.Relation,
			Time: derefFloat(t.Time), Calls: int64(derefFloat(t.Calls)),
		})
	}
	if w.JIT != nil {
		p.JIT = &JIT{
			Functions: intPtr(w.JIT.Functions), Options: w.JIT.Options,
			Inlining:     nested(w.JIT.Timing, "Inlining"),
			Optimization: nested(w.JIT.Timing, "Optimization"),
			Emission:     nested(w.JIT.Timing, "Emission"),
			Generation:   nested(w.JIT.Timing, "Generation"),
			Total:        nested(w.JIT.Timing, "Total"),
		}
	}
	p.Extra = w.Extra

	assignIDs(&p.Root)
	return p, nil
}

// planWrapper is the object EXPLAIN wraps each statement's plan in.
type planWrapper struct {
	Plan            json.RawMessage
	PlanningTime    *float64
	ExecutionTime   *float64
	Settings        map[string]any
	Triggers        []triggerJSON
	JIT             *jitJSON
	QueryIdentifier *int64

	Extra map[string]json.RawMessage
}

// UnmarshalJSON reads the wrapper, keeping whatever it does not model.
func (w *planWrapper) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	known := map[string]func(json.RawMessage) error{
		"Plan":            func(b json.RawMessage) error { w.Plan = b; return nil },
		"Planning Time":   func(b json.RawMessage) error { return json.Unmarshal(b, &w.PlanningTime) },
		"Execution Time":  func(b json.RawMessage) error { return json.Unmarshal(b, &w.ExecutionTime) },
		"Settings":        func(b json.RawMessage) error { return json.Unmarshal(b, &w.Settings) },
		"Triggers":        func(b json.RawMessage) error { return json.Unmarshal(b, &w.Triggers) },
		"JIT":             func(b json.RawMessage) error { return json.Unmarshal(b, &w.JIT) },
		"QueryIdentifier": func(b json.RawMessage) error { return json.Unmarshal(b, &w.QueryIdentifier) },
		// Planning appears alongside Planning Time when BUFFERS is on; it is
		// buffer accounting for the planner itself and is kept as extra.
	}
	for key, b := range raw {
		fn, ok := known[key]
		if !ok {
			if w.Extra == nil {
				w.Extra = make(map[string]json.RawMessage)
			}
			w.Extra[key] = b
			continue
		}
		if err := fn(b); err != nil {
			return fmt.Errorf("reading %q: %w", key, err)
		}
	}
	return nil
}

type triggerJSON struct {
	TriggerName string   `json:"Trigger Name"`
	Relation    string   `json:"Relation"`
	Time        *float64 `json:"Time"`
	Calls       *float64 `json:"Calls"`
}

type jitJSON struct {
	Functions *float64           `json:"Functions"`
	Options   map[string]any     `json:"Options"`
	Timing    map[string]float64 `json:"Timing"`
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func intPtr(p *float64) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

func nested(m map[string]float64, key string) *float64 {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	return &v
}

// assignIDs numbers the nodes breadth-first and records each one's path, so a
// diagnostic can say which node it means.
func assignIDs(root *Node) {
	next := 0
	var visit func(n *Node, path []NodeType)
	visit = func(n *Node, path []NodeType) {
		n.ID = next
		next++
		n.Path = append(append([]NodeType(nil), path...), n.Type)
		for i := range n.Plans {
			visit(&n.Plans[i], n.Path)
		}
	}
	visit(root, nil)
}

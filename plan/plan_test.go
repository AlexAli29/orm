package plan_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/plan"
)

// The plan parser, against JSON rather than against a server.
//
// Everything here is about being wrong later: a node type PostgreSQL has not
// invented yet, a field a newer release adds, a number the server decides to
// quote. A parser that failed on any of them would be a parser nobody could
// upgrade past, so each has a fixture.

func mustParse(t *testing.T, s string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

// A node type from a future PostgreSQL parses like any other, and so does a
// field nobody has modelled.
func TestParse_unknownNodeTypeAndFields(t *testing.T) {
	const future = `[{
	  "Plan": {
	    "Node Type": "Quantum Scan",
	    "Relation Name": "posts",
	    "Total Cost": 12.5,
	    "Plan Rows": 7,
	    "Entanglement Factor": 0.8,
	    "Plans": [
	      {"Node Type": "Seq Scan", "Relation Name": "users", "Total Cost": 1.0,
	       "Future Field": {"nested": true}}
	    ]
	  },
	  "Unknown Top Level": [1, 2, 3]
	}]`

	p := mustParse(t, future)
	if p.Root.Type != "Quantum Scan" {
		t.Errorf("the root is %q", p.Root.Type)
	}
	if p.Root.RelationName != "posts" {
		t.Errorf("the root reads %q", p.Root.RelationName)
	}
	// The unmodelled field is kept rather than dropped.
	if got, ok := p.Root.Extra["Entanglement Factor"]; !ok {
		t.Error("the unknown node field was dropped")
	} else if strings.TrimSpace(string(got)) != "0.8" {
		t.Errorf("the unknown field is %s", got)
	}
	if _, ok := p.Root.Plans[0].Extra["Future Field"]; !ok {
		t.Error("a nested unknown field was dropped")
	}
	if _, ok := p.Extra["Unknown Top Level"]; !ok {
		t.Error("an unknown top-level field was dropped")
	}
	// And an unknown node type is a node like any other.
	if len(p.OfType("Quantum Scan")) != 1 {
		t.Error("the unknown node type is not findable")
	}
	if !p.Root.IsScan() {
		t.Error("a node whose type ends in Scan is not treated as one")
	}
}

// A field the server quotes is still a number.
func TestParse_quotedNumbers(t *testing.T) {
	const quoted = `[{
	  "Plan": {"Node Type": "Seq Scan", "Total Cost": "42.5", "Plan Rows": "17",
	           "Actual Rows": "3", "Actual Loops": "2", "Plan Width": "8"}
	}]`
	p := mustParse(t, quoted)
	if p.Root.TotalCost == nil || *p.Root.TotalCost != 42.5 {
		t.Errorf("the cost is %v", p.Root.TotalCost)
	}
	if p.Root.PlanWidth == nil || *p.Root.PlanWidth != 8 {
		t.Errorf("the width is %v", p.Root.PlanWidth)
	}
	total, ok := p.Root.TotalRows()
	if !ok || total != 6 {
		t.Errorf("three rows over two loops is %v", total)
	}
}

// A missing field and a zero field are different facts.
func TestParse_absentIsNotZero(t *testing.T) {
	planned := mustParse(t, `[{"Plan": {"Node Type": "Seq Scan", "Plan Rows": 0}}]`)
	if planned.Root.PlanRows == nil || *planned.Root.PlanRows != 0 {
		t.Errorf("a planned zero came back as %v", planned.Root.PlanRows)
	}
	if planned.Root.ActualRows != nil {
		t.Error("an unanalysed plan has actual rows")
	}
	if _, ok := planned.Root.TotalRows(); ok {
		t.Error("an unanalysed node reports a row total")
	}
	if planned.Analyzed {
		t.Error("an unanalysed plan says it was analysed")
	}

	analysed := mustParse(t, `[{
	  "Plan": {"Node Type": "Seq Scan", "Actual Rows": 0, "Actual Loops": 1},
	  "Execution Time": 1.5}]`)
	if analysed.Root.ActualRows == nil {
		t.Fatal("an analysed zero came back as absent")
	}
	if total, ok := analysed.Root.TotalRows(); !ok || total != 0 {
		t.Errorf("an analysed zero is %v", total)
	}
	if !analysed.Analyzed {
		t.Error("a plan with an execution time does not say it was analysed")
	}
}

// A JSON null is the same as the key not being there.
func TestParse_nullFields(t *testing.T) {
	p := mustParse(t, `[{"Plan": {"Node Type": "Seq Scan",
	  "Total Cost": null, "Filter": null, "Sort Key": null, "Inner Unique": null}}]`)
	if p.Root.TotalCost != nil {
		t.Error("a null cost became a value")
	}
	if p.Root.Filter != "" {
		t.Errorf("a null filter became %q", p.Root.Filter)
	}
	if p.Root.SortKey != nil {
		t.Error("a null sort key became a slice")
	}
	if p.Root.InnerUnique != nil {
		t.Error("a null Inner Unique became a value")
	}
}

// Nodes are numbered and pathed so a diagnostic can name one.
func TestParse_nodeIdentity(t *testing.T) {
	p := mustParse(t, `[{"Plan": {"Node Type": "Limit", "Plans": [
	  {"Node Type": "Sort", "Plans": [
	    {"Node Type": "Seq Scan", "Relation Name": "posts"},
	    {"Node Type": "Seq Scan", "Relation Name": "users"}]}]}}]`)

	nodes := p.Nodes()
	if len(nodes) != 4 {
		t.Fatalf("the plan has %d nodes, want 4", len(nodes))
	}
	for i, n := range nodes {
		if n.ID != i {
			t.Errorf("node %d has id %d", i, n.ID)
		}
	}
	if p.Depth() != 3 {
		t.Errorf("the plan is %d deep, want 3", p.Depth())
	}

	// The two scans are told apart by their path and their id.
	scans := p.OfType(plan.SeqScan)
	if len(scans) != 2 {
		t.Fatalf("%d scans", len(scans))
	}
	if scans[0].ID == scans[1].ID {
		t.Error("the two scans share an id")
	}
	want := []plan.NodeType{plan.Limit, plan.Sort, plan.SeqScan}
	for i, p := range scans[0].Path {
		if p != want[i] {
			t.Errorf("the first scan's path is %v", scans[0].Path)
			break
		}
	}
	if got, ok := p.Node(2); !ok || got.RelationName != "posts" {
		t.Errorf("node 2 is %+v", got)
	}
	if _, ok := p.Node(99); ok {
		t.Error("a node that does not exist was found")
	}
}

// Self time subtracts the children and accounts for loops.
func TestParse_selfTime(t *testing.T) {
	p := mustParse(t, `[{
	  "Plan": {"Node Type": "Nested Loop", "Actual Total Time": 100, "Actual Loops": 1,
	    "Plans": [
	      {"Node Type": "Seq Scan", "Actual Total Time": 30, "Actual Loops": 1},
	      {"Node Type": "Index Scan", "Actual Total Time": 2, "Actual Loops": 20}]},
	  "Execution Time": 101}]`)

	self, ok := p.Root.SelfTime()
	if !ok {
		t.Fatal("no self time")
	}
	// 100 - 30 - (2 * 20) = 30.
	if self != 30 {
		t.Errorf("the join's own time is %v, want 30", self)
	}
	// The inner scan ran twenty times, so its total is forty milliseconds.
	inner := p.Root.Plans[1]
	if s, _ := inner.SelfTime(); s != 40 {
		t.Errorf("the inner scan's time is %v, want 40", s)
	}

	// A parent that rounds to less than its children reports zero rather than a
	// negative duration.
	odd := mustParse(t, `[{"Plan": {"Node Type": "Limit", "Actual Total Time": 1, "Actual Loops": 1,
	  "Plans": [{"Node Type": "Seq Scan", "Actual Total Time": 5, "Actual Loops": 1}]},
	  "Execution Time": 5}]`)
	if s, _ := odd.Root.SelfTime(); s != 0 {
		t.Errorf("a parent faster than its child reports %v", s)
	}
}

// Buffers and WAL are grouped when the server reported any of their keys, and
// absent when it reported none.
func TestParse_buffersAndWAL(t *testing.T) {
	none := mustParse(t, `[{"Plan": {"Node Type": "Seq Scan"}}]`)
	if none.Root.Buffers != nil {
		t.Error("a plan with no buffer keys has buffer accounting")
	}
	if none.Root.WAL != nil {
		t.Error("a plan with no WAL keys has WAL accounting")
	}

	withBoth := mustParse(t, `[{"Plan": {"Node Type": "ModifyTable",
	  "Shared Hit Blocks": 12, "Shared Read Blocks": 0, "Temp Written Blocks": 5,
	  "I/O Read Time": 1.25,
	  "WAL Records": 3, "WAL FPI": 1, "WAL Bytes": 4096}}]`)
	b := withBoth.Root.Buffers
	if b == nil {
		t.Fatal("no buffer accounting")
	}
	if b.SharedHit == nil || *b.SharedHit != 12 {
		t.Errorf("shared hits: %v", b.SharedHit)
	}
	if b.SharedRead == nil || *b.SharedRead != 0 {
		t.Errorf("a reported zero read came back as %v", b.SharedRead)
	}
	if b.SharedReadTime == nil || *b.SharedReadTime != 1.25 {
		t.Errorf("the I/O read time is %v", b.SharedReadTime)
	}
	w := withBoth.Root.WAL
	if w == nil || w.Records == nil || *w.Records != 3 {
		t.Errorf("WAL: %+v", w)
	}

	// PostgreSQL 17 renamed the I/O timing keys; both spellings land in the same
	// field, because they are the same measurement.
	renamed := mustParse(t, `[{"Plan": {"Node Type": "Seq Scan", "Shared I/O Read Time": 2.5}}]`)
	if renamed.Root.Buffers == nil || renamed.Root.Buffers.SharedReadTime == nil ||
		*renamed.Root.Buffers.SharedReadTime != 2.5 {
		t.Errorf("the renamed I/O timing key did not land: %+v", renamed.Root.Buffers)
	}
}

// The things that are not a plan are refused, and say why.
func TestParse_refusals(t *testing.T) {
	for name, in := range map[string]string{
		"not json":                     `{`,
		"an empty array":               `[]`,
		"two statements":               `[{"Plan":{"Node Type":"A"}},{"Plan":{"Node Type":"B"}}]`,
		"no Plan key":                  `[{"Planning Time": 1}]`,
		"a node with no type":          `[{"Plan": {"Relation Name": "users"}}]`,
		"a node that is not an object": `[{"Plan": 7}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := plan.Parse([]byte(in)); err == nil {
				t.Fatal("this parsed")
			}
		})
	}
	// A single object rather than an array is accepted, because some tools
	// produce it and refusing costs a user something for nothing.
	if _, err := plan.Parse([]byte(`{"Plan": {"Node Type": "Seq Scan"}}`)); err != nil {
		t.Errorf("a bare plan object was refused: %v", err)
	}
}

// The summary is arithmetic over the plan, and says which numbers are its own by
// being a separate type.
func TestParse_summary(t *testing.T) {
	p := mustParse(t, `[{"Plan": {
	  "Node Type": "Gather", "Workers Planned": 4, "Workers Launched": 2,
	  "Plans": [
	    {"Node Type": "Hash Join", "Parallel Aware": true,
	     "Actual Total Time": 58, "Actual Loops": 1, "Plans": [
	      {"Node Type": "Seq Scan", "Relation Name": "posts", "Parallel Aware": true,
	       "Actual Total Time": 50, "Actual Loops": 1},
	      {"Node Type": "Hash", "Hash Batches": 4,
	       "Actual Total Time": 6, "Actual Loops": 1, "Plans": [
	        {"Node Type": "Index Scan", "Relation Name": "users", "Index Name": "users_pkey",
	         "Actual Total Time": 5, "Actual Loops": 1}]}]}],
	  "Actual Total Time": 60, "Actual Loops": 1},
	  "Execution Time": 61}]`)

	s := p.Summarize()
	if s.Nodes != 5 {
		t.Errorf("%d nodes, want 5", s.Nodes)
	}
	if s.Depth != 4 {
		t.Errorf("depth %d, want 4", s.Depth)
	}
	if len(s.Relations) != 2 || s.Relations[0] != "posts" || s.Relations[1] != "users" {
		t.Errorf("relations %v", s.Relations)
	}
	if len(s.Indexes) != 1 || s.Indexes[0] != "users_pkey" {
		t.Errorf("indexes %v", s.Indexes)
	}
	if !s.Parallel {
		t.Error("a plan with parallel-aware nodes is not parallel")
	}
	if s.WorkersPlanned != 4 || s.WorkersLaunched != 2 {
		t.Errorf("workers %d/%d", s.WorkersLaunched, s.WorkersPlanned)
	}
	if len(s.SeqScans) != 1 {
		t.Errorf("%d sequential scans", len(s.SeqScans))
	}
	// The hash spilled to four batches.
	if len(s.Spills) != 1 || s.Spills[0].Type != plan.Hash {
		t.Errorf("spills %v", s.Spills)
	}
	// The slowest node by its own work is the sequential scan.
	if s.SlowestSelf == nil || s.SlowestSelf.RelationName != "posts" {
		t.Errorf("the slowest node is %+v", s.SlowestSelf)
	}
}

// The plan keeps its original JSON, so a field this package has not modelled is
// still reachable without asking the server again.
func TestParse_keepsItsJSON(t *testing.T) {
	const in = `[{"Plan": {"Node Type": "Seq Scan", "Something New": 1}}]`
	p := mustParse(t, in)
	var back []map[string]json.RawMessage
	if err := json.Unmarshal(p.JSON(), &back); err != nil {
		t.Fatalf("the kept JSON does not parse: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("the kept JSON has %d statements", len(back))
	}
	// And it is a copy: editing it does not edit the plan's.
	j := p.JSON()
	for i := range j {
		j[i] = 'x'
	}
	if strings.Contains(string(p.JSON()), "xxx") {
		t.Error("the returned JSON shares storage with the plan")
	}
}

// Rendering is a rendering, and it is readable.
func TestParse_render(t *testing.T) {
	p := mustParse(t, `[{"Plan": {"Node Type": "Limit", "Total Cost": 10, "Plan Rows": 20,
	  "Plans": [{"Node Type": "Seq Scan", "Relation Name": "posts", "Alias": "p",
	             "Filter": "(user_id = $1)", "Total Cost": 9, "Plan Rows": 100}]},
	  "Planning Time": 0.5}]`)
	out := p.String()
	for _, want := range []string{"Limit", "Seq Scan on posts p", "Filter: (user_id = $1)", "Planning Time"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering has no %q:\n%s", want, out)
		}
	}
}

// Walking hands out copies, so a caller cannot edit the plan while reading it.
func TestParse_walkIsReadOnly(t *testing.T) {
	p := mustParse(t, `[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "posts"}}]`)
	p.Walk(func(n plan.Node) { n.RelationName = "edited" })
	if p.Root.RelationName != "posts" {
		t.Errorf("walking edited the plan: %q", p.Root.RelationName)
	}
	nodes := p.Nodes()
	nodes[0].RelationName = "edited"
	if p.Root.RelationName != "posts" {
		t.Errorf("editing a returned node edited the plan: %q", p.Root.RelationName)
	}
}

// A nil plan answers rather than panicking, because a caller who got an error
// and ignored it should get nothing rather than a crash.
func TestParse_nilPlan(t *testing.T) {
	var p *plan.Plan
	p.Walk(func(plan.Node) { t.Error("a nil plan walked a node") })
	if p.Depth() != 0 {
		t.Error("a nil plan has depth")
	}
	if s := p.Summarize(); s.Nodes != 0 {
		t.Error("a nil plan has nodes")
	}
	if p.String() != "" {
		t.Error("a nil plan renders")
	}
	if len(p.Nodes()) != 0 {
		t.Error("a nil plan has nodes")
	}
}

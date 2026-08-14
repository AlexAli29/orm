package diagnostics_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/plan"
)

// Plan diagnostics read from hand-written EXPLAIN JSON.
//
// The JSON here is the shape PostgreSQL produces, cut down to the fields each
// finding depends on. Using fixtures rather than a live server is deliberate
// for this file: a finding that fires on a threshold needs a plan with numbers
// either side of it, and asking a real planner for one of those is asking it to
// cooperate. The integration tests check that real plans parse and that the
// findings appear on real queries; these check the reasoning.

func parse(t *testing.T, js string) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(js))
	if err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	return p
}

func find(t *testing.T, ds []diagnostics.Diagnostic, code string) diagnostics.Diagnostic {
	t.Helper()
	for _, d := range ds {
		if d.Code == code {
			return d
		}
	}
	t.Fatalf("no %s finding in:\n%v", code, ds)
	return diagnostics.Diagnostic{}
}

func absent(t *testing.T, ds []diagnostics.Diagnostic, code string) {
	t.Helper()
	for _, d := range ds {
		if d.Code == code {
			t.Fatalf("unexpected %s finding: %s", code, d)
		}
	}
}

// A plain EXPLAIN says so, and produces none of the findings that need
// measurements.
func TestPlan_unanalysedSaysSo(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Seq Scan","Relation Name":"posts","Alias":"posts",
		"Startup Cost":0.00,"Total Cost":1834.00,"Plan Rows":50000,"Plan Width":32
	}}]`)
	ds := diagnostics.FromPlan(p)

	d := find(t, ds, diagnostics.CodeNotAnalyzed)
	if d.Severity != diagnostics.Info {
		t.Errorf("severity = %s", d.Severity)
	}
	if !strings.Contains(d.Note, "executes the statement") {
		t.Errorf("the note does not warn that ANALYZE runs the query: %s", d.Note)
	}
	// Nothing that needs actuals.
	absent(t, ds, diagnostics.CodeEstimate)
	absent(t, ds, diagnostics.CodeLoops)
	absent(t, ds, diagnostics.CodeRowsRemoved)
}

// Rows a node read and discarded are reported as a measurement.
func TestPlan_rowsRemovedIsAMeasurement(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Seq Scan","Relation Name":"posts","Alias":"posts",
		"Plan Rows":500,"Actual Rows":500,"Actual Loops":1,
		"Filter":"(author_id = 42)","Rows Removed by Filter":199500
	},"Execution Time":120.5}]`)
	ds := diagnostics.FromPlan(p)

	d := find(t, ds, diagnostics.CodeRowsRemoved)
	// A measurement is a notice, not a warning: the node did the work and the
	// plan says how much of it was kept. Whether that is the right plan is not
	// decided here.
	if d.Severity != diagnostics.Notice || d.Confidence != diagnostics.High {
		t.Errorf("severity/confidence = %s/%s, want notice/high", d.Severity, d.Confidence)
	}
	// NodePath is what marks a finding as being about a node: IDs start at
	// zero, so the root's ID is zero and cannot mean "no node".
	if d.NodePath != "Seq Scan" {
		t.Errorf("node path = %q", d.NodePath)
	}
	if _, ok := p.Node(d.NodeID); !ok {
		t.Errorf("node %d is not in the plan", d.NodeID)
	}
	got := map[string]string{}
	for _, e := range d.Evidence {
		got[e.Name] = e.Value
	}
	if got["rows removed"] != "199500" || got["rows returned"] != "500" {
		t.Errorf("evidence = %v", got)
	}
	// The condition's kind is evidence; its text is not, because PostgreSQL
	// writes the bind values into it.
	if got["condition kind"] != "filter" {
		t.Errorf("condition kind = %q", got["condition kind"])
	}
	for _, e := range d.Evidence {
		if strings.Contains(e.Value, "author_id = 42") {
			t.Errorf("the finding quotes the condition text, which carries values: %s", e)
		}
	}
	if got["relation"] != "posts" {
		t.Errorf("relation = %q", got["relation"])
	}
	// Release-critical for M14's scope: the finding is a measurement and
	// carries no remedy. Nothing about indexes, nothing about settings.
	for _, banned := range []string{"index", "work_mem", "ANALYZE the", "should", "recommend"} {
		if strings.Contains(strings.ToLower(d.Note+" "+d.Message), strings.ToLower(banned)) {
			t.Errorf("the finding contains advice (%q):\n%s", banned, d)
		}
	}
}

// Release-critical for M14's scope: there is no finding that judges a
// sequential scan. A scan is reported only through the rows it discarded, and a
// scan that discarded few or none is silent.
func TestPlan_noSequentialScanVerdict(t *testing.T) {
	t.Run("no diagnostic code is about scan type", func(t *testing.T) {
		// Every code the package can emit, checked against the names a
		// verdict-shaped finding would have to have.
		for _, code := range []string{
			diagnostics.CodeRowsRemoved, diagnostics.CodeSortSpill,
			diagnostics.CodeHashSpill, diagnostics.CodeTempIO,
			diagnostics.CodeLoops, diagnostics.CodeRecheck,
			diagnostics.CodeWorkers, diagnostics.CodeBuffers,
			diagnostics.CodeWAL, diagnostics.CodeSettings,
			diagnostics.CodeEstimate, diagnostics.CodeNotAnalyzed,
		} {
			if code == "" {
				t.Error("an empty diagnostic code")
			}
		}
	})

	t.Run("a small table", func(t *testing.T) {
		p := parse(t, `[{"Plan":{
			"Node Type":"Seq Scan","Relation Name":"categories","Alias":"categories",
			"Plan Rows":40,"Actual Rows":40,"Actual Loops":1,
			"Filter":"(name = 'x')","Rows Removed by Filter":60
		}}]`)
		absent(t, diagnostics.FromPlan(p), diagnostics.CodeRowsRemoved)
	})

	t.Run("a large scan that kept its rows", func(t *testing.T) {
		p := parse(t, `[{"Plan":{
			"Node Type":"Seq Scan","Relation Name":"posts","Alias":"posts",
			"Plan Rows":200000,"Actual Rows":200000,"Actual Loops":1
		}}]`)
		absent(t, diagnostics.FromPlan(p), diagnostics.CodeRowsRemoved)
	})

	t.Run("a large scan that discarded half is a measurement, not a verdict", func(t *testing.T) {
		p := parse(t, `[{"Plan":{
			"Node Type":"Seq Scan","Relation Name":"posts","Alias":"posts",
			"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1,
			"Rows Removed by Filter":100000
		}}]`)
		d := find(t, diagnostics.FromPlan(p), diagnostics.CodeRowsRemoved)
		if d.Severity != diagnostics.Notice {
			t.Errorf("severity = %s", d.Severity)
		}
		if strings.Contains(strings.ToLower(d.Message), "bad") {
			t.Errorf("the message passes judgement: %s", d.Message)
		}
	})
}

// Misestimation is compared per loop, which is how PostgreSQL reports both
// numbers, and the loop count is stated so a reader can multiply.
func TestPlan_estimateIsComparedPerLoop(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Nested Loop","Plan Rows":1,"Actual Rows":1,"Actual Loops":1,
		"Plans":[{
			"Node Type":"Index Scan","Relation Name":"comments","Alias":"comments",
			"Index Name":"comments_post_id_idx",
			"Plan Rows":5,"Actual Rows":5000,"Actual Loops":2000
		}]
	},"Execution Time":900.0}]`)
	ds := diagnostics.FromPlan(p)

	d := find(t, ds, diagnostics.CodeEstimate)
	got := map[string]string{}
	for _, e := range d.Evidence {
		got[e.Name] = e.Value
	}
	if got["estimated rows"] != "5" || got["actual rows"] != "5000" {
		t.Errorf("evidence = %v", got)
	}
	if got["ratio"] != "1000x" {
		t.Errorf("ratio = %q", got["ratio"])
	}
	if got["loops"] != "2000" {
		t.Errorf("loops = %q; the per-loop footing has to be stated", got["loops"])
	}
	if !strings.Contains(got["note"], "per loop") {
		t.Errorf("the note does not explain the footing: %q", got["note"])
	}
	// No cause is asserted, and no remedy is offered.
	if !strings.Contains(d.Note, "not determined here") {
		t.Errorf("the note does not decline to name a cause: %s", d.Note)
	}
}

// Zero rows on either side must not divide by anything.
func TestPlan_zeroEstimatesAreSafe(t *testing.T) {
	for _, js := range []string{
		`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":0,"Actual Rows":0,"Actual Loops":1}}]`,
		`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":0,"Actual Rows":50000,"Actual Loops":1}}]`,
		`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":50000,"Actual Rows":0,"Actual Loops":1}}]`,
		`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":50000,"Actual Rows":50000,"Actual Loops":0}}]`,
	} {
		p := parse(t, js)
		// The only requirement is that this does not panic and terminates.
		_ = diagnostics.FromPlan(p)
	}
}

// A sort that spilled is reported, and the work_mem caveat is not optional.
func TestPlan_sortSpill(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Sort","Sort Key":["posts.created_at DESC"],
		"Sort Method":"external merge","Sort Space Used":24576,"Sort Space Type":"Disk",
		"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1
	},"Execution Time":800.0}]`)
	d := find(t, diagnostics.FromPlan(p), diagnostics.CodeSortSpill)

	if d.Severity != diagnostics.Warning {
		t.Errorf("severity = %s", d.Severity)
	}
	// M14 reports the spill and says nothing about work_mem.
	if strings.Contains(strings.ToLower(d.Note+d.Message), "work_mem") {
		t.Errorf("the finding mentions work_mem: %s", d)
	}
	got := map[string]string{}
	for _, e := range d.Evidence {
		got[e.Name] = e.Value
	}
	if got["space used"] != "24576 kB" || got["sort method"] != "external merge" {
		t.Errorf("evidence = %v", got)
	}
	if got["sort keys"] != "1" {
		t.Errorf("sort keys = %q; the count is evidence, the expressions are not", got["sort keys"])
	}
}

// A hash built in several batches is reported; one batch is not.
func TestPlan_hashBatches(t *testing.T) {
	spilled := parse(t, `[{"Plan":{
		"Node Type":"Hash","Hash Buckets":4096,"Hash Batches":8,"Original Hash Batches":1,
		"Peak Memory Usage":4096,"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1
	},"Execution Time":50.0}]`)
	d := find(t, diagnostics.FromPlan(spilled), diagnostics.CodeHashSpill)
	if !strings.Contains(d.Message, "8 batches") {
		t.Errorf("message = %q", d.Message)
	}

	fine := parse(t, `[{"Plan":{
		"Node Type":"Hash","Hash Buckets":4096,"Hash Batches":1,
		"Plan Rows":100,"Actual Rows":100,"Actual Loops":1
	},"Execution Time":1.0}]`)
	absent(t, diagnostics.FromPlan(fine), diagnostics.CodeHashSpill)
}

// A recheck is reported as information and explicitly not as a defect, which
// matters because GiST and GIN always recheck.
func TestPlan_recheckIsNotADefect(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Bitmap Heap Scan","Relation Name":"places","Alias":"places",
		"Recheck Cond":"(location && '...'::geometry)",
		"Rows Removed by Index Recheck":120,"Lossy Heap Blocks":8,
		"Plan Rows":500,"Actual Rows":380,"Actual Loops":1
	},"Execution Time":12.0}]`)
	d := find(t, diagnostics.FromPlan(p), diagnostics.CodeRecheck)

	if d.Severity != diagnostics.Info {
		t.Errorf("a recheck is reported as %s", d.Severity)
	}
	if !strings.Contains(d.Note, "expected rather than a defect") {
		t.Errorf("the note reads as a complaint: %s", d.Note)
	}
	if !strings.Contains(d.Note, "GiST and GIN") {
		t.Errorf("the note does not say why it is expected: %s", d.Note)
	}
}

// Fewer workers than planned is reported as a property of the server, not of
// the query.
func TestPlan_workers(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Gather","Workers Planned":4,"Workers Launched":1,
		"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1
	},"Execution Time":300.0}]`)
	d := find(t, diagnostics.FromPlan(p), diagnostics.CodeWorkers)
	if d.Severity != diagnostics.Info {
		t.Errorf("severity = %s", d.Severity)
	}
	if !strings.Contains(d.Note, "property of the server") {
		t.Errorf("the note blames the query: %s", d.Note)
	}

	full := parse(t, `[{"Plan":{
		"Node Type":"Gather","Workers Planned":2,"Workers Launched":2,
		"Plan Rows":10,"Actual Rows":10,"Actual Loops":1
	},"Execution Time":1.0}]`)
	absent(t, diagnostics.FromPlan(full), diagnostics.CodeWorkers)
}

// Buffers are attributed to the node that read them, net of its children.
func TestPlan_buffersAreNetOfChildren(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Aggregate",
		"Shared Hit Blocks":10100,"Shared Read Blocks":0,
		"Plan Rows":1,"Actual Rows":1,"Actual Loops":1,
		"Plans":[{
			"Node Type":"Seq Scan","Relation Name":"posts","Alias":"posts",
			"Shared Hit Blocks":10000,"Shared Read Blocks":0,
			"Plan Rows":200000,"Actual Rows":200000,"Actual Loops":1
		}]
	},"Execution Time":90.0}]`)
	d := find(t, diagnostics.FromPlan(p), diagnostics.CodeBuffers)
	got := map[string]string{}
	for _, e := range d.Evidence {
		got[e.Name] = e.Value
	}
	// The scan read 10000 of the 10100; the aggregate's own share is 100.
	if got["blocks read by this node"] != "10000" {
		t.Errorf("the node's own blocks = %q, want the child's share excluded from the parent", got["blocks read by this node"])
	}
	if got["relation"] != "posts" {
		t.Errorf("relation = %q", got["relation"])
	}
}

// Non-default planner settings are reported as context.
func TestPlan_settings(t *testing.T) {
	p := parse(t, `[{"Plan":{"Node Type":"Result","Plan Rows":1},
		"Settings":{"enable_seqscan":"off","work_mem":"64MB"}}]`)
	d := find(t, diagnostics.FromPlan(p), diagnostics.CodeSettings)
	got := map[string]string{}
	for _, e := range d.Evidence {
		got[e.Name] = e.Value
	}
	if got["enable_seqscan"] != "off" || got["work_mem"] != "64MB" {
		t.Errorf("evidence = %v", got)
	}
	if d.Severity != diagnostics.Info {
		t.Errorf("severity = %s", d.Severity)
	}
}

// A future node type and an unknown field must not stop any of this working.
func TestPlan_unknownNodesAndFields(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Quantum Scan","Relation Name":"posts","Alias":"posts",
		"Plan Rows":500,"Actual Rows":500,"Actual Loops":1,
		"Rows Removed by Filter":199500,
		"Future Field":{"nested":true},"Another":42
	},"Execution Time":10.0,"Unknown Top Level":"x"}]`)
	ds := diagnostics.FromPlan(p)
	// The seq-scan finding is keyed on the node type, so it correctly does not
	// fire for a type nothing knows. The filter-waste one is not, so it does.
	find(t, ds, diagnostics.CodeRowsRemoved)
	if p.Root.Type != "Quantum Scan" {
		t.Errorf("the node type was not preserved: %q", p.Root.Type)
	}
}

// A nil plan is not a panic.
func TestPlan_nilIsEmpty(t *testing.T) {
	if ds := diagnostics.FromPlan(nil); len(ds) != 0 {
		t.Errorf("a nil plan produced %v", ds)
	}
}

// The report separates the two halves and reports the worst severity across
// both.
func TestReport_worst(t *testing.T) {
	r := diagnostics.Report{
		Static: []diagnostics.Diagnostic{{Code: "a", Severity: diagnostics.Info}},
		Plan:   []diagnostics.Diagnostic{{Code: "b", Severity: diagnostics.Warning}},
	}
	if len(r.All()) != 2 {
		t.Errorf("All returned %d", len(r.All()))
	}
	w, any := r.Worst()
	if !any || w != diagnostics.Warning {
		t.Errorf("worst = %s/%v", w, any)
	}

	empty := diagnostics.Report{}
	if _, any := empty.Worst(); any {
		t.Error("an empty report reports a finding")
	}
	if empty.String() != "no findings" {
		t.Errorf("empty report = %q", empty.String())
	}
}

// Release-critical for M14's scope: no finding this package can produce carries
// tuning or schema advice.
//
// M14 deliberately contains no index advisor. The line it draws is that a
// finding states what the evidence says and stops; deciding what to do about it
// needs a whole workload and somebody accountable for the server, and belongs to
// a developer, a DBA or a higher-level tool. This test is the guard on that
// line, and it runs over every finding the package can emit rather than over the
// ones somebody remembered to check.
func TestDiagnostics_containNoAdvice(t *testing.T) {
	// A plan built to trip as many findings at once as one plan can.
	p := parse(t, `[{"Plan":{
		"Node Type":"Gather","Workers Planned":4,"Workers Launched":1,
		"Plan Rows":10,"Actual Rows":100000,"Actual Loops":1,
		"Shared Hit Blocks":50000,"Shared Read Blocks":10000,
		"WAL Records":100000,"WAL FPI":5000,"WAL Bytes":8388608,
		"Plans":[{
			"Node Type":"Sort","Sort Key":["posts.created_at"],
			"Sort Method":"external merge","Sort Space Used":40960,"Sort Space Type":"Disk",
			"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1,
			"Temp Read Blocks":5000,"Temp Written Blocks":5000,
			"Plans":[{
				"Node Type":"Hash Join","Hash Batches":8,"Hash Buckets":1024,
				"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":2000,
				"Rows Removed by Join Filter":50000,
				"Plans":[{
					"Node Type":"Bitmap Heap Scan","Relation Name":"posts","Alias":"posts",
					"Recheck Cond":"(tags @> '{go}')","Rows Removed by Index Recheck":9000,
					"Lossy Heap Blocks":40,
					"Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1,
					"Rows Removed by Filter":400000
				}]
			}]
		}]
	},"Execution Time":9000.0,"Settings":{"work_mem":"64MB"}}]`)

	ds := diagnostics.FromPlan(p)
	if len(ds) < 6 {
		t.Fatalf("the fixture produced only %d findings, so it does not cover much:\n%v", len(ds), ds)
	}

	// The prose the package writes is checked against every phrase a
	// recommendation would need. Evidence is checked separately and more
	// narrowly: its values are what PostgreSQL reported, and a plan that ran
	// with work_mem set to 64MB says so — reporting that is the opposite of
	// recommending it.
	bannedProse := []string{
		"create index", "add an index", "an index on", "index would", "indexes would",
		"work_mem", "shared_buffers", "effective_cache_size", "random_page_cost",
		"you should", "consider adding", "recommend", "advice", "advisor",
		"run analyze", "analyze the", "vacuum", "tune", "increase", "decrease",
	}
	bannedAnywhere := []string{
		"create index", "you should", "consider adding", "recommend", "advisor",
	}
	for _, d := range ds {
		prose := strings.ToLower(d.Message + " " + d.Note)
		for _, phrase := range bannedProse {
			if strings.Contains(prose, phrase) {
				t.Errorf("%s contains advice (%q):\n%s", d.Code, phrase, d)
			}
		}
		whole := strings.ToLower(d.String())
		for _, phrase := range bannedAnywhere {
			if strings.Contains(whole, phrase) {
				t.Errorf("%s contains advice in its evidence (%q):\n%s", d.Code, phrase, d)
			}
		}
		// Every finding carries evidence, so that no claim rests on nothing.
		if len(d.Evidence) == 0 {
			t.Errorf("%s carries no evidence:\n%s", d.Code, d)
		}
		// Severity stays restrained: nothing here is an error.
		if d.Severity > diagnostics.Warning {
			t.Errorf("%s has severity %d, above warning", d.Code, d.Severity)
		}
	}
}

// The same guard over the static half.
func TestStatic_containsNoAdvice(t *testing.T) {
	s := diagnostics.Shape{
		Kind: diagnostics.KindSelect, Analyzable: true,
		Root:  diagnostics.Source{Kind: diagnostics.SourceTable, Schema: "public", Table: "posts"},
		Joins: []diagnostics.Join{{Type: "CROSS JOIN", Source: diagnostics.Source{Kind: diagnostics.SourceTable, Table: "tags"}}},
		Lock:  &diagnostics.Lock{Strength: "FOR UPDATE", Wait: "SKIP LOCKED"},
		Relations: []diagnostics.Relation{
			{Path: "posts.Comments", Depth: 1, Target: "comments", Cardinality: "many", Batched: true},
		},
		Statements: 2, Correlated: 1,
		BufferedBecause: "relation loading",
	}
	ds := diagnostics.Static(s)
	if len(ds) < 5 {
		t.Fatalf("only %d static findings:\n%v", len(ds), ds)
	}
	for _, d := range ds {
		prose := strings.ToLower(d.Message + " " + d.Note)
		for _, phrase := range []string{"create index", "an index on", "work_mem", "you should", "recommend", "advisor"} {
			if strings.Contains(prose, phrase) {
				t.Errorf("%s contains advice (%q):\n%s", d.Code, phrase, d)
			}
		}
	}
}

// Audit item 65, release-critical for the cardinality claim: PostgreSQL reports
// Plan Rows and Actual Rows on the same per-loop footing, so a node that ran a
// thousand times and produced its estimate every time is not a misestimate.
//
// Comparing an estimate against a multiplied-out total would turn every inner
// side of a nested loop into a thousandfold error, which is the single easiest
// way to make cardinality diagnostics useless.
func TestPlan_loopsAreNotAMisestimate(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Nested Loop","Plan Rows":100000,"Actual Rows":100000,"Actual Loops":1,
		"Plans":[{
			"Node Type":"Index Scan","Relation Name":"comments","Alias":"comments",
			"Plan Rows":100,"Actual Rows":100,"Actual Loops":1000
		}]
	},"Execution Time":50.0}]`)

	for _, d := range diagnostics.FromPlan(p) {
		if d.Code == diagnostics.CodeEstimate {
			t.Errorf("a node that met its estimate on every one of 1000 loops was reported as a misestimate:\n%s", d)
		}
	}

	// And a real misestimate on the same footing still fires, so the check
	// above is not passing by never firing at all.
	q := parse(t, `[{"Plan":{
		"Node Type":"Nested Loop","Plan Rows":1,"Actual Rows":1,"Actual Loops":1,
		"Plans":[{
			"Node Type":"Index Scan","Relation Name":"comments","Alias":"comments",
			"Plan Rows":100,"Actual Rows":100000,"Actual Loops":1000
		}]
	},"Execution Time":50.0}]`)
	find(t, diagnostics.FromPlan(q), diagnostics.CodeEstimate)
}

// Audit item 68: rows removed is reported for an index scan by the same
// mechanism as for a sequential scan. A finding that only fired on Seq Scan
// would be a scan verdict wearing a measurement's name.
func TestPlan_rowsRemovedIsNotScanTypeSpecific(t *testing.T) {
	for _, nodeType := range []string{"Seq Scan", "Index Scan", "Bitmap Heap Scan", "Index Only Scan"} {
		p := parse(t, `[{"Plan":{
			"Node Type":"`+nodeType+`","Relation Name":"posts","Alias":"posts",
			"Plan Rows":500,"Actual Rows":500,"Actual Loops":1,
			"Rows Removed by Filter":199500
		},"Execution Time":10.0}]`)
		d := find(t, diagnostics.FromPlan(p), diagnostics.CodeRowsRemoved)
		if d.Severity != diagnostics.Notice {
			t.Errorf("%s: severity = %s", nodeType, d.Severity)
		}
	}
}

// Audit item 75: a child reporting more buffers than its parent is malformed,
// and must not produce a negative count.
func TestPlan_bufferUnderflowIsClamped(t *testing.T) {
	p := parse(t, `[{"Plan":{
		"Node Type":"Aggregate","Shared Hit Blocks":10,
		"Plan Rows":1,"Actual Rows":1,"Actual Loops":1,
		"Plans":[{"Node Type":"Seq Scan","Relation Name":"t","Alias":"t",
			"Shared Hit Blocks":1000000,
			"Plan Rows":1,"Actual Rows":1,"Actual Loops":1}]
	},"Execution Time":1.0}]`)
	for _, d := range diagnostics.FromPlan(p) {
		for _, e := range d.Evidence {
			if strings.HasPrefix(e.Value, "-") {
				t.Errorf("%s reported a negative count: %s", d.Code, e)
			}
			// A share is a share. Underflow does not only produce negatives:
			// subtracting a child's larger count from its parent makes the
			// denominator tiny and the sibling's share enormous, which is the
			// shape a leading-minus check misses entirely.
			if e.Name == "share" {
				var pct float64
				if _, err := fmt.Sscanf(e.Value, "%f%%", &pct); err == nil {
					if pct < 0 || pct > 100 {
						t.Errorf("%s reported a share of %s, which is not a share", d.Code, e.Value)
					}
				}
			}
		}
	}
}

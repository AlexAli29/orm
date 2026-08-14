package diagnostics_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/plan"
)

// FuzzPlanParse throws arbitrary bytes at the plan parser.
//
// The properties are the ones a decoder for somebody else's output has to hold:
// it never panics, it rejects what is not an EXPLAIN result rather than
// returning half of one, and it tolerates fields and node types it has never
// heard of — because PostgreSQL will add both and a parser that refused them
// would break on an upgrade.
func FuzzPlanParse(f *testing.F) {
	f.Add([]byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":1}}]`))
	f.Add([]byte(`[{"Plan":{"Node Type":"Future Scan","Whatever":{"a":[1,2,3]}},"New Top":1}]`))
	f.Add([]byte(`[{"Plan":{"Node Type":"Nested Loop","Plans":[{"Node Type":"Seq Scan"}]}}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`[{"Plan":{"Node Type":"Seq Scan","Plan Rows":1e309}}]`))
	f.Add([]byte(`[{"Plan":{"Node Type":"Sort","Sort Space Used":-1}}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// A guard on the input rather than on the parser: deeply nested JSON is
		// the encoding/json package's problem, and generating megabytes of it
		// measures nothing about this code.
		if len(data) > 64<<10 {
			t.Skip()
		}
		p, err := plan.Parse(data)
		if err != nil {
			if p != nil {
				t.Fatalf("a failed parse returned a plan: %v", err)
			}
			return
		}
		if p == nil {
			t.Fatal("a successful parse returned no plan")
		}

		// Everything downstream of a parsed plan must survive it too. This is
		// where a diagnostic dividing by a zero the fuzzer supplied would show.
		_ = p.Nodes()
		_ = p.Depth()
		_ = p.Summarize()
		_ = p.String()

		for _, d := range diagnostics.FromPlan(p) {
			if d.Code == "" {
				t.Errorf("a finding with no code: %s", d)
			}
			// No arithmetic escaped into the rendering.
			text := d.String()
			for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
				if strings.Contains(text, bad) {
					t.Errorf("a finding rendered %s:\n%s", bad, d)
				}
			}
		}
	})
}

// FuzzDiagnosticsNumbers builds plans out of adversarial numbers directly,
// which reaches arithmetic that valid-looking JSON rarely does.
//
// Zero rows, zero loops, enormous counts and absent fields are all legal in a
// plan PostgreSQL can produce, and each is a way to divide by nothing.
func FuzzDiagnosticsNumbers(f *testing.F) {
	f.Add(0.0, 0.0, 0.0, 0.0, int64(0))
	f.Add(1.0, 1e12, 1e9, 5e8, int64(1<<40))
	f.Add(-1.0, -1.0, -1.0, -1.0, int64(-5))
	f.Add(1e308, 1e308, 1e308, 1e308, int64(math.MaxInt64))

	f.Fuzz(func(t *testing.T, planRows, actualRows, loops, removed float64, sortSpace int64) {
		for _, v := range []float64{planRows, actualRows, loops, removed} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Skip() // JSON cannot carry these, so PostgreSQL cannot send them.
			}
		}
		js := fmt.Sprintf(`[{"Plan":{
			"Node Type":"Seq Scan","Relation Name":"t","Alias":"t",
			"Plan Rows":%s,"Actual Rows":%s,"Actual Loops":%s,
			"Rows Removed by Filter":%s,
			"Sort Space Used":%d,"Sort Space Type":"Disk","Sort Method":"external merge",
			"Hash Batches":4,"Workers Planned":4,"Workers Launched":1,
			"Shared Hit Blocks":10,"Temp Read Blocks":5000,
			"Plans":[{"Node Type":"Index Scan","Relation Name":"u","Alias":"u",
				"Plan Rows":%s,"Actual Rows":%s,"Actual Loops":%s}]
		},"Execution Time":1.0}]`,
			num(planRows), num(actualRows), num(loops), num(removed), sortSpace,
			num(actualRows), num(planRows), num(loops))

		p, err := plan.Parse([]byte(js))
		if err != nil {
			return // a number JSON would not accept
		}
		for _, d := range diagnostics.FromPlan(p) {
			text := d.String()
			for _, bad := range []string{"NaN", "Inf"} {
				if strings.Contains(text, bad) {
					t.Fatalf("%s rendered %s from planRows=%v actualRows=%v loops=%v removed=%v:\n%s",
						d.Code, bad, planRows, actualRows, loops, removed, d)
				}
			}
			if len(d.Evidence) == 0 {
				t.Errorf("%s carries no evidence", d.Code)
			}
		}
	})
}

// num renders a float the way JSON accepts it, so the fixture stays parseable
// for everything except the values JSON genuinely cannot express.
func num(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return "0"
	}
	return string(b)
}

// FuzzStaticShape throws arbitrary structure at the static half.
//
// A Shape is built by this module rather than by a caller, so what this checks
// is robustness against the shapes composition can actually produce — an empty
// root, no joins, a lock with no targets, relation steps with odd depths — and
// that no finding is ever produced without evidence or with an empty code.
func FuzzStaticShape(f *testing.F) {
	f.Add(0, 0, 0, false, false, false, "")
	f.Add(3, 2, 5, true, true, true, "users.Posts")
	f.Add(-1, -1, -1, false, true, false, "")

	f.Fuzz(func(t *testing.T, joins, ctes, relations int, limit, lock, streams bool, path string) {
		if joins > 64 || ctes > 64 || relations > 64 || len(path) > 256 {
			t.Skip()
		}
		s := diagnostics.Shape{
			Kind: diagnostics.KindSelect, Analyzable: true,
			Root:     diagnostics.Source{Kind: diagnostics.SourceTable, Table: "t"},
			HasLimit: limit, Streams: streams,
		}
		for i := 0; i < joins; i++ {
			s.Joins = append(s.Joins, diagnostics.Join{Type: "JOIN", HasCondition: i%2 == 0})
		}
		for i := 0; i < ctes; i++ {
			s.CTEs = append(s.CTEs, diagnostics.Source{Kind: diagnostics.SourceCTE, Name: "c"})
		}
		for i := 0; i < relations; i++ {
			s.Relations = append(s.Relations, diagnostics.Relation{
				Path: path, Depth: i, Target: "x", Cardinality: "many", Batched: i%2 == 0,
			})
			s.Statements++
		}
		if lock {
			s.Lock = &diagnostics.Lock{Strength: "FOR UPDATE"}
		}

		for _, d := range diagnostics.Static(s) {
			if d.Code == "" {
				t.Errorf("a finding with no code: %s", d)
			}
			if d.Severity > diagnostics.Warning {
				t.Errorf("%s has severity above warning", d.Code)
			}
			// The no-advice rule holds for every shape, not just the ones a
			// hand-written test thought of.
			prose := strings.ToLower(d.Message + " " + d.Note)
			for _, banned := range []string{"create index", "an index on", "work_mem", "recommend"} {
				if strings.Contains(prose, banned) {
					t.Errorf("%s contains advice (%q)", d.Code, banned)
				}
			}
		}
	})
}

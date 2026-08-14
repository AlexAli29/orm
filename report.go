package orm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/plan"
)

// The performance report.
//
// It gathers what the other parts of M14 already produce — the fingerprint, the
// statement's structure, PostgreSQL's plan and the findings read from both —
// into one value, and renders it for a person when asked.
//
// # It does not execute the statement
//
// [PerformanceReport] uses plain EXPLAIN. PostgreSQL plans the statement and
// throws the plan away without running it, so a report on an UPDATE changes no
// rows and a report on a DELETE deletes none. That is not a detail: a
// convenience method that quietly ran the statement it was asked to describe
// would be the single most dangerous thing in this package, and a permanent
// test asserts the row counts either side of one.
//
// Runtime measurements need [PerformanceReportAnalyze], whose name says what it
// does, or a plan obtained separately from [Query.ExplainAnalyze] and handed to
// [ReportFromPlan].
//
// # The plan is PostgreSQL's text, and PostgreSQL puts values in it
//
// A plan is not value-free and cannot be made so. PostgreSQL plans a
// parameterised statement with the values it was given — a custom plan — and
// writes them into the conditions it reports:
//
//	Index Cond: (email = 'someone@example.com'::text)
//
// That string is the server's output, not this package's, and removing the
// literal from it would mean parsing SQL, which this package does not do.
//
// So the boundary is drawn where it can be held. [Report.Plan] holds
// PostgreSQL's plan exactly as it arrived, conditions and all, because a report
// that quietly altered the server's answer would be worse than one that carries
// it. What [Report.String] renders leaves the condition strings out, so the
// rendering — the thing that ends up in a log, a bug report or a terminal — has
// nowhere for a value to appear. [WithConditions] turns them back on for a
// reader who wants them and knows what is in them.
//
// # It reports facts
//
// There is no advice in a report and no index advisor behind it. The sections
// are provenance-separated on purpose — what the ORM knows structurally, what
// PostgreSQL estimated, what PostgreSQL measured, and what was derived from
// those — so that a reader, or a tool, can tell which is which and reason on the
// evidence rather than on this package's opinion.

// Provenance says where a part of a report came from, which is the thing a
// reader most needs to know before trusting a number.
type Provenance string

// The provenances.
const (
	// FromORM is a structural fact read from the query the ORM compiled. It is
	// certain and needs no database.
	FromORM Provenance = "orm"
	// FromEstimate is PostgreSQL's own guess, produced by planning the
	// statement without running it.
	FromEstimate Provenance = "postgresql estimate"
	// FromActual is what PostgreSQL measured while running the statement.
	FromActual Provenance = "postgresql measurement"
	// FromDerived is computed here from one of the above. The evidence on each
	// finding says which numbers went into it.
	FromDerived Provenance = "derived"
)

// Report is everything M14 can say about one statement.
//
// Every field is structured. The rendering [Report.String] produces is built
// from these values and is for people; anything reading a report
// programmatically should read the fields, so that a tool never has to parse
// terminal output.
type Report struct {
	// Fingerprint identifies the statement's shape. Two executions with
	// different bind values share one.
	Fingerprint Fingerprint
	// SQL is the statement as it would be sent, with its placeholders still in
	// it. There are no bind values here or anywhere else in a report.
	SQL string
	// Args is how many arguments the statement carries. The values are
	// deliberately absent.
	Args int

	// Shape is the ORM's structural reading of the statement.
	Shape diagnostics.Shape
	// Static holds the findings read from that structure.
	Static []diagnostics.Diagnostic

	// Plan is PostgreSQL's plan, or nil when none was obtained.
	//
	// It is the server's answer verbatim, which means it carries the constants
	// PostgreSQL planned with: a condition on a parameterised statement reads
	// (email = 'someone@example.com') even though the statement sent a
	// placeholder. That is deliberate — altering the server's answer would make
	// the field useless for the thing it is for — and it is why encoding a
	// report does not encode this field. See [Report.MarshalJSON].
	Plan *plan.Plan `json:"-"`
	// PlanSummary is the derived summary of that plan.
	PlanSummary *plan.Summary
	// PlanFindings holds the findings read from the plan.
	PlanFindings []diagnostics.Diagnostic

	// Analyzed reports whether the plan carries measurements. It is false for
	// every report produced by [PerformanceReport], which does not execute the
	// statement.
	Analyzed bool
}

// RenderOption configures how a report renders.
type RenderOption func(*renderConfig)

type renderConfig struct{ conditions bool }

// WithConditions renders the plan's condition strings — Filter, Index Cond and
// the rest — which are left out by default.
//
// They are left out because PostgreSQL writes the statement's bind values into
// them, so a rendering that included them would carry whatever the query was
// filtering on into wherever the rendering went. Turning them on is reasonable
// when reading a plan by hand; it is not reasonable in a log.
func WithConditions() RenderOption { return func(c *renderConfig) { c.conditions = true } }

// MarshalJSON encodes the report with the plan redacted.
//
// The rendering withholds the plan's condition strings, and encoding has to
// withhold them too or the guarantee would hold only for the output nobody
// machine-reads. A structured report is exactly the thing that ends up in a log
// pipeline, so what it encodes is [plan.Plan.Redacted]: every node type,
// relation, index, cost, row count, loop count, buffer and timing, without the
// free-form expression text that can carry a caller's data.
//
// [Report.Plan] still holds the server's answer verbatim for a caller who wants
// it and knows what is in it. Encoding that is then an explicit act:
//
//	json.Marshal(report.Plan)
//
// rather than something that happens by encoding the report.
func (r Report) MarshalJSON() ([]byte, error) {
	type alias Report // avoids recursing into this method
	return json.Marshal(struct {
		alias
		// RedactedPlan is the plan with its expression text removed. The name
		// says what it is, so nothing reading this mistakes it for the server's
		// verbatim answer.
		RedactedPlan *plan.Plan `json:"RedactedPlan,omitempty"`
	}{
		alias:        alias(r),
		RedactedPlan: r.Plan.Redacted(),
	})
}

// Diagnostics returns the two halves of the report as one diagnostics report.
func (r Report) Diagnostics() diagnostics.Report {
	return diagnostics.Report{Static: r.Static, Plan: r.PlanFindings}
}

// PerformanceReport builds a report for a statement without executing it.
//
// It compiles the statement, fingerprints it, reads its structure, asks
// PostgreSQL to plan it with a plain EXPLAIN, and reads that plan. Nothing here
// runs the statement: an UPDATE described by this function updates nothing.
//
// It is a function rather than a method so that every statement kind reaches it
// the same way, through the [Statement] the rest of the package already builds.
func PerformanceReport(ctx context.Context, ex Executor, s Statement, shape diagnostics.Shape, opts ...ExplainOption) (Report, error) {
	r, err := staticReport(s, shape)
	if err != nil {
		return r, err
	}
	// Explain, never ExplainAnalyze. This call is the whole safety property of
	// the default report and is the thing the mutation test watches.
	p, err := Explain(ctx, ex, s, opts...)
	if err != nil {
		return r, err
	}
	r.attach(p)
	return r, nil
}

// PerformanceReportAnalyze builds a report from a plan that carries
// measurements, and **executes the statement to get one**.
//
// The name is long and says ANALYZE because the behaviour is dangerous. For a
// SELECT it costs the query's execution; for an INSERT, UPDATE or DELETE it
// performs the write, and for a statement calling a volatile function it
// performs whatever that function does. Rolling it back protects PostgreSQL's
// own transactional effects and nothing else:
//
//	tx, err := db.Begin(ctx)
//	report, err := orm.PerformanceReportAnalyze(ctx, tx, stmt, shape)
//	tx.Rollback(ctx)
//
// Nothing inside this package calls it.
func PerformanceReportAnalyze(ctx context.Context, ex Executor, s Statement, shape diagnostics.Shape, opts ...ExplainOption) (Report, error) {
	r, err := staticReport(s, shape)
	if err != nil {
		return r, err
	}
	p, err := ExplainAnalyze(ctx, ex, s, opts...)
	if err != nil {
		return r, err
	}
	r.attach(p)
	return r, nil
}

// ReportFromPlan builds a report around a plan obtained earlier.
//
// It is how a plan captured an hour ago, read from a log, or produced by
// [Query.ExplainAnalyze] in a place where the danger was considered becomes a
// report. It contacts nothing.
func ReportFromPlan(s Statement, shape diagnostics.Shape, p *plan.Plan) (Report, error) {
	r, err := staticReport(s, shape)
	if err != nil {
		return r, err
	}
	r.attach(p)
	return r, nil
}

// staticReport is everything that needs no database.
func staticReport(s Statement, shape diagnostics.Shape) (Report, error) {
	r := Report{Shape: shape, Static: diagnostics.Static(shape)}
	if s == nil {
		return r, nil
	}
	sql, args, err := s.SQL()
	if err != nil {
		return r, err
	}
	r.SQL, r.Args = sql, len(args)
	if fp, ferr := FingerprintOf(s); ferr == nil {
		r.Fingerprint = fp
	}
	return r, nil
}

// attach records a plan and everything read from it.
func (r *Report) attach(p *plan.Plan) {
	if p == nil {
		return
	}
	r.Plan = p
	r.Analyzed = p.Analyzed
	sum := p.Summarize()
	r.PlanSummary = &sum
	r.PlanFindings = diagnostics.FromPlan(p)
}

// String renders the report for a person.
//
// It is built from the fields above and adds nothing: anything reading a report
// in a program should read those fields rather than this text, which exists to
// be looked at rather than parsed.
func (r Report) String() string { return r.Render() }

// Render is [Report.String] with options.
func (r Report) Render(opts ...RenderOption) string {
	var cfg renderConfig
	for _, o := range opts {
		o(&cfg)
	}
	var b strings.Builder

	b.WriteString("Statement\n")
	fmt.Fprintf(&b, "  kind: %s\n", r.Shape.Kind)
	if r.Shape.Analyzable && r.Shape.Root.Table != "" {
		fmt.Fprintf(&b, "  root: %s\n", r.Shape.Root)
	}
	if !r.Fingerprint.IsZero() {
		fmt.Fprintf(&b, "  fingerprint: %s\n", r.Fingerprint)
	}
	fmt.Fprintf(&b, "  arguments: %d (values are not shown)\n", r.Args)

	if r.SQL != "" {
		b.WriteString("\nSQL (placeholders, not values)\n")
		for _, line := range strings.Split(r.SQL, "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	b.WriteString("\nStatic diagnostics — " + string(FromORM) + "\n")
	writeFindings(&b, r.Static)

	switch {
	case r.Plan == nil:
		b.WriteString("\nPlan\n  none: no plan was obtained\n")
	default:
		source := FromEstimate
		if r.Analyzed {
			source = FromActual
		}
		fmt.Fprintf(&b, "\nPlan — %s\n", source)
		renderNode(&b, r.Plan.Root, 1, cfg.conditions)
		if !r.Analyzed {
			b.WriteString("  (row counts are the planner's estimates; the statement was not executed)\n")
		}
	}

	if r.PlanSummary != nil {
		fmt.Fprintf(&b, "\nPlan summary — %s\n", FromDerived)
		s := *r.PlanSummary
		fmt.Fprintf(&b, "  nodes: %d\n  depth: %d\n", s.Nodes, s.Depth)
		if len(s.Relations) > 0 {
			fmt.Fprintf(&b, "  relations: %s\n", strings.Join(s.Relations, ", "))
		}
		if len(s.Indexes) > 0 {
			fmt.Fprintf(&b, "  indexes used: %s\n", strings.Join(s.Indexes, ", "))
		}
		if len(s.ScanTypes) > 0 {
			kinds := make([]string, 0, len(s.ScanTypes))
			for t, n := range s.ScanTypes {
				kinds = append(kinds, fmt.Sprintf("%s x%d", t, n))
			}
			// The map is iterated, so the order is sorted before rendering:
			// a report that changed between runs would be unreadable in a diff.
			sort.Strings(kinds)
			fmt.Fprintf(&b, "  scans: %s\n", strings.Join(kinds, ", "))
		}
		if s.Parallel {
			fmt.Fprintf(&b, "  workers: %d planned, %d launched\n", s.WorkersPlanned, s.WorkersLaunched)
		}
		if len(s.Spills) > 0 {
			fmt.Fprintf(&b, "  nodes that spilled to disk: %d\n", len(s.Spills))
		}
	}

	if len(r.PlanFindings) > 0 {
		fmt.Fprintf(&b, "\nPlan diagnostics — %s\n", FromDerived)
		writeFindings(&b, r.PlanFindings)
	}

	b.WriteString("\nThis report states facts. It contains no index or configuration advice:\n" +
		"deciding what to change needs a whole workload, and belongs to you.\n")
	return b.String()
}

// renderNode writes the plan tree, leaving out the condition strings unless
// they were asked for.
//
// It renders here rather than calling the plan's own String because that one
// prints the conditions, which is right for somebody looking at a plan and
// wrong for a report that may be logged.
func renderNode(b *strings.Builder, n plan.Node, depth int, conditions bool) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s%s", indent, n.Type)
	if t := n.Target(); t != "" {
		fmt.Fprintf(b, " on %s", t)
	}
	if n.TotalCost != nil {
		fmt.Fprintf(b, "  (cost=%.2f", *n.TotalCost)
		if n.PlanRows != nil {
			fmt.Fprintf(b, " rows=%.0f", *n.PlanRows)
		}
		b.WriteString(")")
	}
	if n.ActualRows != nil {
		fmt.Fprintf(b, " (actual rows=%.0f", *n.ActualRows)
		if n.ActualLoops != nil {
			fmt.Fprintf(b, " loops=%.0f", *n.ActualLoops)
		}
		b.WriteString(")")
	}
	b.WriteString("\n")

	for _, c := range n.Conditions() {
		if !conditions {
			// The kind is safe — it says an index condition exists — and the
			// text is not, because PostgreSQL wrote the bind value into it.
			fmt.Fprintf(b, "%s  %s: (withheld; use WithConditions to render it)\n", indent, c.Kind)
			continue
		}
		fmt.Fprintf(b, "%s  %s: %s\n", indent, c.Kind, c.Cond)
	}
	for _, child := range n.Plans {
		renderNode(b, child, depth+1, conditions)
	}
}

func writeFindings(b *strings.Builder, ds []diagnostics.Diagnostic) {
	if len(ds) == 0 {
		b.WriteString("  none\n")
		return
	}
	for _, d := range ds {
		fmt.Fprintf(b, "  %s %s: %s\n", d.Code, d.Severity, d.Message)
		if d.NodePath != "" {
			fmt.Fprintf(b, "      at: %s\n", d.NodePath)
		}
		for _, e := range d.Evidence {
			fmt.Fprintf(b, "      %s\n", e)
		}
		if d.Note != "" {
			fmt.Fprintf(b, "      note: %s\n", d.Note)
		}
	}
}

// PerformanceReport describes the query without running it.
//
// It uses a plain EXPLAIN, so a report costs a planning round trip and nothing
// else. Use [Query.PerformanceReportAnalyze] for measurements, which executes
// the query to obtain them.
func (q *Query[E]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := q.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReport(ctx, q.repo.ex, q, shape, opts...)
}

// PerformanceReportAnalyze describes the query and **executes it** to measure it.
func (q *Query[E]) PerformanceReportAnalyze(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := q.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReportAnalyze(ctx, q.repo.ex, q, shape, opts...)
}

// PerformanceReport describes the projection without running it.
func (q *SelectQuery[E, R]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := q.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReport(ctx, q.repo.ex, q, shape, opts...)
}

// PerformanceReport describes the composed query without running it.
func (q *ComposedQuery[R]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := q.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReport(ctx, q.ex, q, shape, opts...)
}

// PerformanceReport describes the UPDATE without running it.
//
// The statement is planned and the plan discarded: no row is updated. Only
// [PerformanceReportAnalyze] performs the write, and its name says so.
func (u *Update[E]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := u.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReport(ctx, u.repo.ex, u, shape, opts...)
}

// PerformanceReport describes the DELETE without running it. No row is deleted.
func (d *Delete[E]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, err := d.Shape()
	if err != nil {
		return Report{}, err
	}
	return PerformanceReport(ctx, d.repo.ex, d, shape, opts...)
}

// PerformanceReport describes a raw statement without running it.
//
// The structural half is nearly empty — a raw statement's SQL was written by the
// caller and this package does not parse SQL — but the fingerprint, the plan and
// the plan findings are all as good as any other statement's.
func (q *RawQuery[E]) PerformanceReport(ctx context.Context, opts ...ExplainOption) (Report, error) {
	shape, _ := q.Shape()
	return PerformanceReport(ctx, q.repo.ex, q, shape, opts...)
}

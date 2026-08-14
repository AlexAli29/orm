// Package diagnostics turns what the ORM already knows about a statement into
// structured findings a person can act on.
//
// There are two kinds, and keeping them apart is the point. A static diagnostic
// is read from the query's own structure — its sources, its joins, whether it
// streams, what it locks — and needs no database. A plan diagnostic is read
// from what PostgreSQL said about the statement, and is only as good as the
// plan it was given: a plain EXPLAIN carries estimates, and only EXPLAIN ANALYZE
// carries what actually happened.
//
// # This package reports facts, and stops there
//
// PostgreSQL is the planner. What this package does is notice things that are
// visible in evidence the server produced, say which evidence, and stop.
//
// It stops deliberately and completely. There is no index advisor here and
// there is no tuning advice: nothing recommends creating an index, changing a
// schema, running ANALYZE or raising work_mem. Those are decisions that need a
// whole workload, a maintenance window and somebody accountable for the
// server, and this package has none of the three — it is looking at one
// statement. A sequential scan is reported because it happened, not because it
// is wrong; it is very often the fastest plan available.
//
// The [Diagnostic.Note] field is explanatory rather than remedial. It says what
// a number means — what a recheck is, what a loop count is measured per — so
// that a reader can interpret the evidence. It never says what to do about it.
// Deciding that is the developer's, the DBA's, or a higher-level tool's job,
// and separating the facts from the reasoning is what makes those decisions
// possible on evidence rather than on this package's opinion.
//
// # Severity and confidence are two different questions
//
// Severity is how much the finding matters. Confidence is how sure the finding
// is. They come apart constantly: "this sort spilled to disk" is a fact read
// straight out of the plan and so is certain, but may not matter; "this
// sequential scan might want an index" can matter a great deal and still be a
// guess. A report that collapsed them would have to round one of them off, and
// rounding confidence up is how a tool starts lying.
//
// Nothing here is a correctness error. The most severe finding this package
// produces is a warning, because a slow query is still a correct one.
package diagnostics

import (
	"fmt"
	"strings"
)

// Severity is how much a finding matters.
type Severity uint8

// The severities, least first.
const (
	// Info states a fact about the statement or its plan. It is not a
	// suggestion and needs no action.
	Info Severity = iota
	// Notice is something worth knowing about, which may or may not be worth
	// changing.
	Notice
	// Warning is evidence of real cost. It is the most severe finding this
	// package produces: a slow query is not an incorrect one.
	Warning
)

// String returns the severity's name.
func (s Severity) String() string {
	switch s {
	case Warning:
		return "warning"
	case Notice:
		return "notice"
	default:
		return "info"
	}
}

// Confidence is how sure a finding is, which is a different question from how
// much it matters.
type Confidence uint8

// The confidence levels, least first.
const (
	// Low means a heuristic with incomplete evidence. Read the evidence before
	// acting on it.
	Low Confidence = iota
	// Medium means the structure supports the finding but the runtime evidence
	// that would settle it is missing — usually because the plan was not
	// analysed.
	Medium
	// High means the finding is read directly out of evidence the server
	// produced, or follows from the query's structure alone.
	High
)

// String returns the confidence's name.
func (c Confidence) String() string {
	switch c {
	case High:
		return "high"
	case Medium:
		return "medium"
	default:
		return "low"
	}
}

// Evidence is one fact a finding rests on, named so that a reader can check it.
//
// A finding without evidence is an opinion. Every diagnostic this package
// produces carries the numbers or the structure it was derived from, so that
// somebody who disagrees can see exactly what was read and where.
type Evidence struct {
	// Name is what was measured, as a person would say it: "rows removed by
	// filter", "relation", "sort method".
	Name string
	// Value is what it was. It is a string because evidence is for reading;
	// anything that needs to be computed on should come from the plan.
	Value string
}

// String renders the evidence as "name: value", which is the form the report
// prints it in.
func (e Evidence) String() string { return e.Name + ": " + e.Value }

// ev is the internal shorthand for building evidence.
func ev(name string, value any) Evidence {
	return Evidence{Name: name, Value: fmt.Sprint(value)}
}

// Diagnostic is one finding: what was noticed, how much it matters, how sure it
// is, and the evidence it was read from.
type Diagnostic struct {
	// Code identifies the kind of finding and never changes for a given kind,
	// so that a project can suppress or track one without matching on prose.
	Code string
	// Severity is how much it matters; Confidence is how sure it is.
	Severity   Severity
	Confidence Confidence
	// Message is one sentence stating the finding.
	Message string
	// Evidence lists what the finding was read from.
	Evidence []Evidence
	// Note explains what the evidence means, and is empty when the message and
	// the evidence already say everything.
	//
	// It is deliberately not a recommendation. A note may say that PostgreSQL
	// reports a node's time per loop, or that a GiST recheck is expected rather
	// than a defect; it will not say to add an index or to change a setting.
	// See the package documentation for why that line is where it is.
	Note string
	// NodeID names the plan node the finding is about, and is the ID
	// [plan.Plan.Node] takes. Plan node IDs start at zero — the root is node
	// zero — so it is NodePath, not NodeID, that says whether a finding is
	// about a node at all.
	NodeID int
	// NodePath is the node's position in the tree, rendered for reading:
	// "Limit -> Sort -> Seq Scan". It is empty exactly when the finding is not
	// about one node: every static finding, and the plan findings that are
	// about the statement as a whole.
	NodePath string
}

// String renders the diagnostic on one line, followed by its evidence.
func (d Diagnostic) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (%s confidence)", d.Severity, d.Message, d.Confidence)
	if d.NodePath != "" {
		fmt.Fprintf(&b, "\n    at: %s", d.NodePath)
	}
	for _, e := range d.Evidence {
		fmt.Fprintf(&b, "\n    %s", e)
	}
	if d.Note != "" {
		fmt.Fprintf(&b, "\n    note: %s", d.Note)
	}
	return b.String()
}

// The diagnostic codes.
//
// They are grouped by where the finding came from: QS for a static reading of
// the query's structure, PL for a reading of PostgreSQL's plan. A code never
// changes meaning, so a project can suppress one by code and keep that
// suppression across upgrades.
const (
	// Static findings, read from the query's own structure.

	// CodeShape states the statement's kind, sources and structure.
	CodeShape = "QS001"
	// CodeRawBoundary says a Raw statement cannot be analysed structurally.
	CodeRawBoundary = "QS002"
	// CodeCrossJoin reports a join with no condition.
	CodeCrossJoin = "QS003"
	// CodeUnbounded reports a row-returning query with no LIMIT.
	CodeUnbounded = "QS004"
	// CodeLocking reports a locking clause.
	CodeLocking = "QS005"
	// CodeBuffered says the operation reads every row before returning.
	CodeBuffered = "QS006"
	// CodeRelationPlan reports how many statements a relation load will run.
	CodeRelationPlan = "QS007"
	// CodeCorrelated reports a correlated subquery.
	CodeCorrelated = "QS008"

	// Plan findings, read from what PostgreSQL reported.

	// CodeEstimate reports a large difference between estimated and actual rows.
	CodeEstimate = "PL001"
	// CodeRowsRemoved reports rows a node read and then discarded, which is a
	// measurement rather than a judgement: the node did the work and the plan
	// says how much of it was kept.
	CodeRowsRemoved = "PL002"
	// PL003 was a second rows-discarded finding that split scans from
	// everything above them. It was retired into CodeRowsRemoved, and the code
	// is not reused: a code that changed meaning would silently invalidate
	// whatever a project had suppressed under it.
	// CodeSortSpill reports a sort that used disk.
	CodeSortSpill = "PL004"
	// CodeHashSpill reports a hash that needed more than one batch.
	CodeHashSpill = "PL005"
	// CodeTempIO reports temporary block I/O.
	CodeTempIO = "PL006"
	// CodeLoops reports a node executed many times.
	CodeLoops = "PL007"
	// CodeRecheck reports an index recheck, which is normal for GiST and GIN.
	CodeRecheck = "PL008"
	// CodeWorkers reports fewer parallel workers launched than planned.
	CodeWorkers = "PL009"
	// CodeBuffers reports the nodes that read the most buffers.
	CodeBuffers = "PL010"
	// CodeWAL reports write-ahead log volume.
	CodeWAL = "PL011"
	// CodeSettings reports non-default planner settings in force.
	CodeSettings = "PL012"
	// CodeNotAnalyzed says the plan carries estimates rather than measurements.
	CodeNotAnalyzed = "PL013"
)

// Report is everything this package found about one statement.
type Report struct {
	// Static holds the findings read from the query's structure.
	Static []Diagnostic
	// Plan holds the findings read from PostgreSQL's plan, and is empty when
	// no plan was supplied.
	Plan []Diagnostic
}

// All returns every finding, static first.
func (r Report) All() []Diagnostic {
	out := make([]Diagnostic, 0, len(r.Static)+len(r.Plan))
	out = append(out, r.Static...)
	return append(out, r.Plan...)
}

// Worst returns the highest severity in the report, and whether there was any
// finding at all.
func (r Report) Worst() (Severity, bool) {
	all := r.All()
	if len(all) == 0 {
		return Info, false
	}
	worst := all[0].Severity
	for _, d := range all[1:] {
		if d.Severity > worst {
			worst = d.Severity
		}
	}
	return worst, true
}

// String renders the whole report.
func (r Report) String() string {
	all := r.All()
	if len(all) == 0 {
		return "no findings"
	}
	parts := make([]string, 0, len(all))
	for _, d := range all {
		parts = append(parts, d.Code+" "+d.String())
	}
	return strings.Join(parts, "\n")
}

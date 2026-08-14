package diagnostics

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/AlexAli29/orm/plan"
)

// Reading PostgreSQL's plan.
//
// # Condition strings are not evidence here
//
// PostgreSQL writes a statement's bind values into the conditions it reports —
// "Index Cond: (email = 'someone@example.com'::text)" — because it planned the
// statement with the values it was given. A finding that quoted that string
// would carry whatever the query filtered on into wherever the finding went.
//
// So the evidence names which kind of condition a node carried and never its
// text. That is enough to say what the node was doing, and it is the difference
// between a finding that is safe to log and one that is not. The condition
// itself is still in the plan, which is PostgreSQL's own output and is kept
// verbatim.
//
// Everything here is derived from fields the server reported. Where a field is
// absent — because BUFFERS was not asked for, because the plan was not analysed
// — the finding that would have needed it is not produced rather than guessed
// at. That is why so much of this is written as "if the pointer is nil, return".
//
// # Thresholds
//
// Several findings need a line drawn somewhere, and any line is arguable. The
// ones below were chosen to be quiet: they are set where a person looking at
// the plan would already have noticed the same thing, so that a report full of
// findings means something. They are named constants rather than literals so
// that the choice is visible and reviewable, and each says what it is for.
//
// None of them is a claim about what is fast. A sequential scan of a small
// table is not a problem, a sort that spilled is not necessarily a problem, and
// a plan with no findings is not necessarily good. The thresholds decide what
// is worth *reporting*, not what is worth *changing* — nothing here recommends
// a change.
const (
	// estimateFactor is how far a row estimate has to be out before it is worth
	// reporting. An order of magnitude is where the planner's choice of join
	// strategy starts to be driven by the wrong number.
	estimateFactor = 10
	// estimateFloor keeps the ratio test off tiny numbers, where being "ten
	// times out" means one row against ten.
	estimateFloor = 100
	// filterWasteRows is how many rows a node has to discard before the
	// discarding is worth mentioning at all.
	filterWasteRows = 10_000
	// filterWasteFraction is the share of what a node read that it has to throw
	// away before that is interesting. Reading twice what you keep is normal.
	filterWasteFraction = 0.9
	// seqScanRows is how large a sequential scan has to be before an index is
	// even a question. Below this the scan is very likely the right plan and a
	// finding would be noise.
	seqScanRows = 10_000
	// loopAmplification is how many times a node has to run underneath its
	// parent before the repetition is worth pointing at.
	loopAmplification = 1_000
	// tempBlocks is how many temporary blocks of I/O are worth reporting.
	tempBlocks = 1_000
	// bufferShare is the fraction of a plan's total buffer reads one node has
	// to account for before it is called out as the dominant one.
	bufferShare = 0.5
	// walBytes is how much write-ahead log a statement has to generate before
	// the volume is worth reporting.
	walBytes = 1 << 20
)

// FromPlan reads PostgreSQL's plan and reports what it says.
//
// The findings differ in kind depending on what the plan carries. An unanalysed
// plan — the one plain EXPLAIN returns — has estimates and no measurements, so
// what can be said about it is structural and is marked as such. An analysed
// plan carries what actually happened, and the findings from it are facts.
//
// It never executes anything and never contacts a server: the plan is the whole
// input.
func FromPlan(p *plan.Plan) []Diagnostic {
	if p == nil {
		return nil
	}
	var out []Diagnostic

	if !p.Analyzed {
		out = append(out, Diagnostic{
			Code: CodeNotAnalyzed, Severity: Info, Confidence: High,
			Message: "the plan carries PostgreSQL's estimates rather than measurements",
			Evidence: []Evidence{
				ev("source", "EXPLAIN without ANALYZE"),
			},
			Note: "ExplainAnalyze reports what actually happened instead, and executes the statement to find out",
		})
	}

	out = append(out, estimateFindings(p)...)
	out = append(out, rowsRemovedFindings(p)...)
	out = append(out, spillFindings(p)...)
	out = append(out, loopFindings(p)...)
	out = append(out, recheckFindings(p)...)
	out = append(out, workerFindings(p)...)
	out = append(out, bufferFindings(p)...)
	out = append(out, walFindings(p)...)
	out = append(out, settingsFinding(p)...)
	return out
}

// nodePath renders where a node sits, root first, so a reader can find it.
func nodePath(n plan.Node) string {
	if len(n.Path) == 0 {
		return string(n.Type)
	}
	parts := make([]string, 0, len(n.Path))
	for _, t := range n.Path {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, " -> ")
}

// at fills in the node identity every plan finding carries.
func at(d Diagnostic, n plan.Node) Diagnostic {
	d.NodeID = n.ID
	d.NodePath = nodePath(n)
	return d
}

// estimateFindings compares what the planner expected against what it got.
//
// Both numbers are per loop, which is how PostgreSQL reports them: a node
// inside a nested loop that ran a thousand times reports the average of one
// execution on each side. Comparing a per-loop estimate against a total would
// manufacture a misestimate out of arithmetic, which is why the two are taken
// from the same footing and the loop count is reported alongside.
func estimateFindings(p *plan.Plan) []Diagnostic {
	if !p.Analyzed {
		return nil
	}
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		if n.PlanRows == nil || n.ActualRows == nil {
			return
		}
		est, act := *n.PlanRows, *n.ActualRows
		// A node that never ran has nothing to compare. PostgreSQL reports
		// zero loops for a subplan the executor skipped.
		if n.ActualLoops != nil && *n.ActualLoops == 0 {
			return
		}
		if !finite(est) || !finite(act) {
			return
		}
		if est < estimateFloor && act < estimateFloor {
			return
		}
		// The ratio is taken in whichever direction is out, with both sides
		// floored at one so that an estimate of zero rows is comparable rather
		// than a division by it.
		lo, hi := est, act
		if lo > hi {
			lo, hi = hi, lo
		}
		if lo < 1 {
			lo = 1
		}
		ratio := hi / lo
		if !finite(ratio) || ratio < estimateFactor {
			return
		}

		direction := "under"
		if est > act {
			direction = "over"
		}
		evidence := []Evidence{
			ev("estimated rows", fmt.Sprintf("%.0f", est)),
			ev("actual rows", fmt.Sprintf("%.0f", act)),
			ev("ratio", fmt.Sprintf("%.0fx", ratio)),
		}
		if n.ActualLoops != nil {
			evidence = append(evidence, ev("loops", fmt.Sprintf("%.0f", *n.ActualLoops)),
				Evidence{Name: "note", Value: "both row counts are per loop, as PostgreSQL reports them"})
		}
		if t := n.Target(); t != "" {
			evidence = append(evidence, ev("relation", t))
		}
		out = append(out, at(Diagnostic{
			Code: CodeEstimate, Severity: Notice, Confidence: High,
			Message:  fmt.Sprintf("%s estimated its row count %sby about %.0fx", n.Type, direction+" ", ratio),
			Evidence: evidence,
			// No cause is asserted and no remedy is offered. Which of the
			// several possible causes this is would need the catalog, the data
			// and the query's parameters, and this function has none of them;
			// what it has is the two numbers, which is what it reports.
			Note: "the planner's estimate and the executor's count differ by this much; the cause is not determined here",
		}, n))
	})
	return out
}

// rowsRemovedFindings reports work a node did and then discarded.
//
// This is a measurement, not a verdict. A node that reads a million rows and
// keeps five did that reading, and the plan says so; whether that is the right
// plan depends on what indexes exist, how big the relation is, what else runs
// on the server and what the query is for. None of that is decided here, and in
// particular a sequential scan is reported because it happened rather than
// because it is wrong — for many relations and many predicates it is the
// fastest thing PostgreSQL has.
func rowsRemovedFindings(p *plan.Plan) []Diagnostic {
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		removed := deref(n.RowsRemovedByFilter) + deref(n.RowsRemovedByJoin)
		if removed < filterWasteRows {
			return
		}
		kept, ok := n.TotalRows()
		if !ok {
			return
		}
		loops := 1.0
		if n.ActualLoops != nil && *n.ActualLoops > 0 {
			loops = *n.ActualLoops
		}
		// Rows removed is reported per loop, like every other row count, so it
		// is multiplied out to the same footing as the rows that were kept.
		removedTotal := removed * loops
		total := kept + removedTotal
		if total <= 0 || !finite(removedTotal) || !finite(total) || !finite(kept) {
			return
		}

		evidence := []Evidence{
			ev("rows returned", fmt.Sprintf("%.0f", kept)),
			ev("rows removed", fmt.Sprintf("%.0f", removedTotal)),
		}
		if share := removedTotal / total * 100; finite(share) {
			evidence = append(evidence, ev("removed share", fmt.Sprintf("%.0f%%", share)))
		}
		if n.RowsRemovedByFilter != nil {
			evidence = append(evidence, ev("rows removed by filter (per loop)", fmt.Sprintf("%.0f", *n.RowsRemovedByFilter)))
		}
		if n.RowsRemovedByJoin != nil {
			evidence = append(evidence, ev("rows removed by join filter (per loop)", fmt.Sprintf("%.0f", *n.RowsRemovedByJoin)))
		}
		if loops > 1 {
			evidence = append(evidence, ev("loops", fmt.Sprintf("%.0f", loops)))
		}
		if kind, cond := n.Condition(); cond != "" {
			// The kind, never the text: the text has the bind values in it.
			evidence = append(evidence, ev("condition kind", strings.ToLower(kind)))
		}
		if t := n.Target(); t != "" {
			evidence = append(evidence, ev("relation", t))
		}
		out = append(out, at(Diagnostic{
			Code: CodeRowsRemoved, Severity: Notice, Confidence: High,
			Message:  fmt.Sprintf("%s returned %.0f rows and removed %.0f", n.Type, kept, removedTotal),
			Evidence: evidence,
			Note:     "PostgreSQL reports removed rows per loop; the totals above are multiplied out",
		}, n))
	})
	return out
}

// spillFindings reports sorts and hashes that did not fit in memory.
//
// They are reported because they happened and because the plan measured them,
// not as a prompt to change a setting. work_mem is deliberately not mentioned:
// it applies per sort per connection, so what it should be is a question about
// a whole server under its real concurrency, and this function is looking at
// one node of one statement.
func spillFindings(p *plan.Plan) []Diagnostic {
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		if n.SortSpaceType == "Disk" || strings.Contains(strings.ToLower(n.SortMethod), "external") {
			evidence := []Evidence{ev("sort method", n.SortMethod)}
			if n.SortSpaceUsed != nil {
				evidence = append(evidence, ev("space used", fmt.Sprintf("%d kB", *n.SortSpaceUsed)))
			}
			if len(n.SortKey) > 0 {
				// How many keys, not which: a sort key is an expression and an
				// expression can carry a literal.
				evidence = append(evidence, ev("sort keys", len(n.SortKey)))
			}
			out = append(out, at(Diagnostic{
				Code: CodeSortSpill, Severity: Warning, Confidence: High,
				Message:  "a sort did not fit in memory and used disk",
				Evidence: evidence,
				Note:     "PostgreSQL reports the sort method it chose and how much disk it used",
			}, n))
		}

		if n.HashBatches != nil && *n.HashBatches > 1 {
			evidence := []Evidence{ev("batches", *n.HashBatches)}
			if n.OriginalHashBatches != nil {
				evidence = append(evidence, ev("batches planned", *n.OriginalHashBatches))
			}
			if n.PeakMemoryUsage != nil {
				evidence = append(evidence, ev("peak memory", fmt.Sprintf("%d kB", *n.PeakMemoryUsage)))
			}
			out = append(out, at(Diagnostic{
				Code: CodeHashSpill, Severity: Notice, Confidence: High,
				Message:  fmt.Sprintf("a hash was built in %d batches rather than one", *n.HashBatches),
				Evidence: evidence,
				Note:     "more than one batch means the hash side was written out and read back rather than held in memory",
			}, n))
		}

		if n.Buffers != nil {
			temp := deref64(n.Buffers.TempRead) + deref64(n.Buffers.TempWritten)
			if temp >= tempBlocks {
				out = append(out, at(Diagnostic{
					Code: CodeTempIO, Severity: Notice, Confidence: High,
					Message: fmt.Sprintf("%s used %d temporary blocks", n.Type, temp),
					Evidence: []Evidence{
						ev("temp blocks read", deref64(n.Buffers.TempRead)),
						ev("temp blocks written", deref64(n.Buffers.TempWritten)),
					},
					Note: "temporary blocks are work written to disk, usually by a sort, a hash or a materialised intermediate result",
				}, n))
			}
		}
	})
	return out
}

// loopFindings reports a node run very many times.
//
// A nested loop is not a defect: for a small outer side and an indexed inner
// side it is the fastest thing PostgreSQL has. What is worth pointing at is the
// repetition itself, with the per-loop cost beside it, so a reader can multiply.
func loopFindings(p *plan.Plan) []Diagnostic {
	if !p.Analyzed {
		return nil
	}
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		if n.ActualLoops == nil || !finite(*n.ActualLoops) || *n.ActualLoops < loopAmplification {
			return
		}
		evidence := []Evidence{ev("loops", fmt.Sprintf("%.0f", *n.ActualLoops))}
		if n.ActualTotalTime != nil {
			evidence = append(evidence, ev("time per loop", fmt.Sprintf("%.3f ms", *n.ActualTotalTime)))
			if total := *n.ActualTotalTime * *n.ActualLoops; finite(total) {
				evidence = append(evidence, ev("time in total", fmt.Sprintf("%.3f ms", total)))
			}
		}
		if t := n.Target(); t != "" {
			evidence = append(evidence, ev("relation", t))
		}
		out = append(out, at(Diagnostic{
			Code: CodeLoops, Severity: Notice, Confidence: High,
			Message:  fmt.Sprintf("%s ran %.0f times", n.Type, *n.ActualLoops),
			Evidence: evidence,
			Note: "PostgreSQL reports this node's time and rows per loop; the total above is multiplied out. " +
				"Running many times is what a nested loop's inner side does and says nothing on its own about whether the plan is right",
		}, n))
	})
	return out
}

// recheckFindings surfaces index rechecks without calling any of them a defect.
//
// GiST and GIN are lossy by design: the index answers "these rows might match"
// and the heap settles it. That is how a spatial index and a JSONB index work
// at all, so a recheck is expected and is reported as information. What is
// worth seeing is how much of the rechecked work was discarded.
func recheckFindings(p *plan.Plan) []Diagnostic {
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		removed := deref(n.RowsRemovedByIndexRecheck) + deref(n.RowsRemovedByRecheck)
		lossy := deref(n.LossyHeapBlocks)
		if n.RecheckCond == "" && removed == 0 && lossy == 0 {
			return
		}
		evidence := []Evidence{}
		if n.RecheckCond != "" {
			evidence = append(evidence, ev("recheck", "present"))
		}
		if removed > 0 {
			evidence = append(evidence, ev("rows removed by recheck", fmt.Sprintf("%.0f", removed)))
		}
		if lossy > 0 {
			evidence = append(evidence, ev("lossy heap blocks", fmt.Sprintf("%.0f", lossy)))
		}
		if kept, ok := n.TotalRows(); ok {
			evidence = append(evidence, ev("rows kept", fmt.Sprintf("%.0f", kept)))
		}
		out = append(out, at(Diagnostic{
			Code: CodeRecheck, Severity: Info, Confidence: High,
			Message:  fmt.Sprintf("%s rechecked its index results against the heap", n.Type),
			Evidence: evidence,
			Note: "a recheck is expected rather than a defect: GiST and GIN answer which rows might match and the heap settles it, " +
				"which is how spatial, range and JSONB indexes work at all",
		}, n))
	})
	return out
}

// workerFindings reports parallel workers that were planned but not launched.
//
// Getting fewer than planned is a fact about the server's worker pool at that
// moment, not about the query, so this is information rather than a suggestion.
func workerFindings(p *plan.Plan) []Diagnostic {
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		if n.WorkersPlanned == nil || n.WorkersLaunched == nil {
			return
		}
		if *n.WorkersLaunched >= *n.WorkersPlanned {
			return
		}
		out = append(out, at(Diagnostic{
			Code: CodeWorkers, Severity: Info, Confidence: High,
			Message: fmt.Sprintf("%d of %d planned parallel workers were launched", *n.WorkersLaunched, *n.WorkersPlanned),
			Evidence: []Evidence{
				ev("workers planned", *n.WorkersPlanned),
				ev("workers launched", *n.WorkersLaunched),
			},
			Note: "the server had fewer parallel workers free than the plan asked for, which is a property of the server at that moment rather than of this statement",
		}, n))
	})
	return out
}

// bufferFindings names the node that did most of the reading, when BUFFERS was
// asked for and one node dominates.
func bufferFindings(p *plan.Plan) []Diagnostic {
	var total int64
	p.Walk(func(n plan.Node) {
		if n.Buffers != nil {
			total += selfBuffers(n)
		}
	})
	if total == 0 {
		return nil
	}
	var out []Diagnostic
	p.Walk(func(n plan.Node) {
		if n.Buffers == nil {
			return
		}
		self := selfBuffers(n)
		if self == 0 || float64(self)/float64(total) < bufferShare {
			return
		}
		evidence := []Evidence{
			ev("blocks read by this node", self),
			ev("blocks read by the statement", total),
			ev("share", fmt.Sprintf("%.0f%%", float64(self)/float64(total)*100)),
			ev("shared hit", deref64(n.Buffers.SharedHit)),
			ev("shared read", deref64(n.Buffers.SharedRead)),
		}
		if t := n.Target(); t != "" {
			evidence = append(evidence, ev("relation", t))
		}
		out = append(out, at(Diagnostic{
			Code: CodeBuffers, Severity: Info, Confidence: High,
			Message:  fmt.Sprintf("%s accounts for most of the statement's buffer traffic", n.Type),
			Evidence: evidence,
			Note:     "shared hit is a block found in the buffer cache and shared read is one that was not",
		}, n))
	})
	return out
}

// walFindings reports how much write-ahead log a statement generated.
func walFindings(p *plan.Plan) []Diagnostic {
	var records, fpi int64
	var bytes float64
	p.Walk(func(n plan.Node) {
		if n.WAL == nil {
			return
		}
		records += deref64(n.WAL.Records)
		fpi += deref64(n.WAL.FPI)
		bytes += deref(n.WAL.Bytes)
	})
	if !finite(bytes) || bytes < walBytes {
		return nil
	}
	return []Diagnostic{{
		Code: CodeWAL, Severity: Notice, Confidence: High,
		Message: fmt.Sprintf("the statement generated %.1f MiB of write-ahead log", bytes/(1<<20)),
		Evidence: []Evidence{
			ev("WAL records", records),
			ev("WAL full-page images", fpi),
			ev("WAL bytes", fmt.Sprintf("%.0f", bytes)),
		},
		Note: "full-page images are written the first time a page changes after a checkpoint, so the count depends on when the statement ran relative to one",
	}}
}

// settingsFinding reports the non-default planner settings that were in force,
// as context for everything else in the plan.
func settingsFinding(p *plan.Plan) []Diagnostic {
	if len(p.Settings) == 0 {
		return nil
	}
	names := make([]string, 0, len(p.Settings))
	for k := range p.Settings {
		names = append(names, k)
	}
	sort.Strings(names)
	evidence := make([]Evidence, 0, len(names))
	for _, k := range names {
		evidence = append(evidence, ev(k, p.Settings[k]))
	}
	return []Diagnostic{{
		Code: CodeSettings, Severity: Info, Confidence: High,
		Message:  fmt.Sprintf("%d planner settings differ from their defaults", len(names)),
		Evidence: evidence,
		Note:     "these were in force when the plan was made, and account for plan choices that look surprising without them",
	}}
}

// scanned is how many rows a node examined: what it returned plus what its
// filter threw away, totalled across loops.
func scanned(n plan.Node) (float64, bool) {
	kept, ok := n.TotalRows()
	if !ok {
		return 0, false
	}
	loops := 1.0
	if n.ActualLoops != nil && *n.ActualLoops > 0 {
		loops = *n.ActualLoops
	}
	return kept + deref(n.RowsRemovedByFilter)*loops, true
}

// selfBuffers is the blocks a node read on its own behalf. PostgreSQL reports
// buffers cumulatively, so this subtracts the children.
func selfBuffers(n plan.Node) int64 {
	own := deref64(n.Buffers.SharedHit) + deref64(n.Buffers.SharedRead) +
		deref64(n.Buffers.LocalHit) + deref64(n.Buffers.LocalRead)
	for _, c := range n.Plans {
		if c.Buffers != nil {
			own -= deref64(c.Buffers.SharedHit) + deref64(c.Buffers.SharedRead) +
				deref64(c.Buffers.LocalHit) + deref64(c.Buffers.LocalRead)
		}
	}
	if own < 0 {
		return 0
	}
	return own
}

// finite reports whether a computed number is one a report can print.
//
// The plan's numbers come from JSON somebody else produced, and a plan read
// from a log or written by a proxy can carry values whose product overflows.
// Multiplying two of those gives +Inf, dividing two infinities gives NaN, and a
// report saying a node discarded NaN% of its rows is worse than one that says
// nothing. So every derived number is checked before it is used, and a finding
// that cannot be computed reports the measurements it does have.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

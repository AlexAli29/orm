package reconcile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Dependency reconciliation.
//
// # The contract, stated once so nothing has to infer it
//
// Desired dependencies are the direct relation dependencies a declaration
// states with //orm:depends-on. Actual dependencies are the direct relation
// dependencies PostgreSQL reports for the stored query, after the foundation's
// filtering: relations only, self excluded, extension-owned objects excluded.
//
// Both sides are direct and neither is a closure. A view reading a view depends
// on that view and not on what it reads — flattening either side would make a
// two-level graph compare as a one-level one, and the ordering a planner builds
// from it would be right by accident.
//
// Identity is schema and name, always. An unqualified comparison would make
// public.users and archive.users one relation, which is exactly the mistake
// that produces a migration ordered against the wrong table.
//
// # Why both directions are errors
//
// A dependency set is not documentation. It is the input to migration ordering,
// and ordering is a total order over a graph: an edge the graph does not have
// is as wrong as one it is missing. A relation that reads something no
// declaration mentions will be created before that thing exists. A declared
// edge the relation does not have constrains the order for no reason, and —
// more to the point — means the committed graph is not the one the database
// has, so nothing built from it can be trusted. Reporting one and not the other
// would leave the graph half-checked.

// CheckDependencies compares declared dependencies against the ones PostgreSQL
// reports, for every managed view in the desired schema.
//
// Findings are added in a fixed order — by relation, then by the dependency's
// own qualified name — so that two runs over one schema render identically
// whatever order the catalog returned its rows in.
func CheckDependencies(report *diag.Report, desired, actual *schema.Schema) {
	if desired == nil || actual == nil {
		return
	}
	type rel struct {
		qualified string
		kind      string
		desired   []schema.RelationRef
		actual    []schema.RelationRef
		found     bool
	}
	byName := map[string]*rel{}
	var order []string

	add := func(q, kind string, deps []schema.RelationRef) {
		if _, ok := byName[q]; !ok {
			byName[q] = &rel{qualified: q, kind: kind}
			order = append(order, q)
		}
		byName[q].desired = deps
	}
	for _, v := range desired.Views {
		add(v.Qualified(), "view", v.DependsOn)
	}
	for _, m := range desired.MaterializedViews {
		add(m.Qualified(), "materialized view", m.DependsOn)
	}
	for _, v := range actual.Views {
		if r, ok := byName[v.Qualified()]; ok {
			r.actual, r.found = v.DependsOn, true
		}
	}
	for _, m := range actual.MaterializedViews {
		if r, ok := byName[m.Qualified()]; ok {
			r.actual, r.found = m.DependsOn, true
		}
	}

	// Sorted, though diag.Report sorts its findings too. This is belt and
	// braces rather than the mechanism: the report's own ordering is what
	// guarantees a deterministic rendering, and this keeps the traversal from
	// depending on map iteration in case anything ever reads findings in the
	// order they were added.
	slices.Sort(order)
	for _, q := range order {
		r := byName[q]
		if !r.found {
			// The relation is not there. Its absence is the entity tier's
			// finding, and a dependency report about a relation that does not
			// exist would be a second, less useful answer to the same question.
			continue
		}
		want := names(r.desired)
		have := names(r.actual)

		for _, extra := range missingFrom(have, want) {
			report.Add(diag.Finding{
				Code: diag.E033,
				Message: fmt.Sprintf("%s reads %s, and no declaration says so",
					r.qualified, extra),
				Reason: "dependencies order migrations. A relation that reads something the " +
					"declared graph does not contain can be created before that thing exists, " +
					"and the failure appears when the migration runs on an empty database rather " +
					"than here",
				Fix: fmt.Sprintf("add //orm:depends-on %s to the declaration of %s, or change "+
					"the definition so it no longer reads it", extra, r.qualified),
				Table: r.qualified,
			})
		}
		for _, stale := range missingFrom(want, have) {
			report.Add(diag.Finding{
				Code: diag.E034,
				Message: fmt.Sprintf("%s declares a dependency on %s, which it does not read",
					r.qualified, stale),
				Reason: "the committed dependency graph is not the one the database has. The SQL " +
					"still works, and the order a planner builds from this graph is derived from " +
					"something untrue — which is the kind of difference that stays invisible until " +
					"an unrelated change makes the order matter",
				Fix: fmt.Sprintf("remove //orm:depends-on %s from the declaration of %s, or change "+
					"the definition to read it", stale, r.qualified),
				Table: r.qualified,
			})
		}
	}
}

// names renders a dependency set as sorted qualified names.
func names(refs []schema.RelationRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Qualified())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// missingFrom returns the entries of a that are not in b.
func missingFrom(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// CheckMetadata reports relation metadata the schema model cannot express.
//
// Silence here would be the dangerous answer. A view carries options that
// decide whose privileges its base tables are read with, and a schema that
// omitted one while reporting the relation clean would be stating that it had
// checked something it cannot represent. Reporting it costs a warning and
// leaves the decision where it belongs.
func CheckMetadata(report *diag.Report, actual *schema.Schema, managed map[string]bool) {
	if actual == nil {
		return
	}
	views := slices.Clone(actual.Views)
	schema.SortViews(views)
	for _, v := range views {
		if !managed[v.Qualified()] {
			continue
		}
		var unrepresented []string
		for _, o := range v.Options {
			// The managed declaration has no syntax for any of these yet, so
			// every one present in the database is something the desired schema
			// cannot state and a migration would not reproduce.
			unrepresented = append(unrepresented, o.Name+"="+o.Value)
		}
		if len(unrepresented) == 0 {
			continue
		}
		slices.Sort(unrepresented)
		report.Add(diag.Finding{
			Code: diag.W035,
			Message: fmt.Sprintf("%s carries %s, which a managed declaration cannot express",
				v.Qualified(), strings.Join(unrepresented, ", ")),
			Reason: "these are not decoration. security_invoker decides whose privileges the " +
				"underlying tables are read with, and a check option decides whether a write " +
				"through the view may produce a row it cannot see. Managed mode has no syntax " +
				"for them, so a migration recreating this view would not reproduce them",
			Fix: fmt.Sprintf("keep %s outside managed reconciliation, or set the option again "+
				"by hand after any migration that recreates it", v.Qualified()),
			Table: v.Qualified(),
		})
	}
}

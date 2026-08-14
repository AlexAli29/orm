package reconcile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Materialized-view index reconciliation.
//
// The comparison is schema.Diff's, over schema.Table values the materialized
// views lend for the duration. That is deliberate: an index on a materialized
// view is an index, the differ already knows what makes two of them different —
// key order, method, predicate, INCLUDE, operator class, uniqueness — and a
// second comparison written here would be a second thing to keep in agreement
// with PostgreSQL. When the differ learns about a new index property, this
// learns about it too, because there is nothing here to teach.

// CheckMaterializedIndexes compares each managed materialized view's declared
// indexes against the ones the database has.
func CheckMaterializedIndexes(report *diag.Report, want, actual *schema.Schema) {
	if want == nil || actual == nil {
		return
	}
	have := map[string]schema.MaterializedView{}
	for _, m := range actual.MaterializedViews {
		have[m.Qualified()] = m
	}

	mats := slices.Clone(want.MaterializedViews)
	schema.SortMaterializedViews(mats)
	for _, w := range mats {
		a, ok := have[w.Qualified()]
		if !ok {
			// The relation is absent, which the entity tier reports. Index
			// findings about a relation that is not there would be noise about
			// a fact already stated more usefully.
			continue
		}
		// Two tables carrying only the indexes, so the differ compares exactly
		// those and nothing it would find missing on a view — no primary key,
		// no foreign keys, no columns to report as dropped.
		wantT := schema.Table{Schema: w.Schema, Name: w.Name, Indexes: w.Indexes}
		haveT := schema.Table{Schema: a.Schema, Name: a.Name, Indexes: a.Indexes}
		wantT.Normalize()
		haveT.Normalize()

		for _, d := range schema.Diff(
			&schema.Schema{Tables: []schema.Table{haveT}},
			&schema.Schema{Tables: []schema.Table{wantT}},
		) {
			if !strings.Contains(d, "index") {
				// The lent tables carry nothing but indexes, so anything else
				// would be a difference in a field neither side set.
				continue
			}
			report.Add(diag.Finding{
				Code:    diag.E036,
				Message: fmt.Sprintf("%s: %s", w.Qualified(), d),
				Reason: "a materialized view's indexes are ordinary PostgreSQL indexes, and this " +
					"one differs from the declaration. A unique index over plain columns is also " +
					"what REFRESH CONCURRENTLY requires, so an index that drifts can take a " +
					"concurrent refresh with it",
				Fix: fmt.Sprintf("bring the declaration and %s into agreement; the difference is "+
					"stated above in the schema's own vocabulary", w.Qualified()),
				Table: w.Qualified(),
			})
		}
	}
}

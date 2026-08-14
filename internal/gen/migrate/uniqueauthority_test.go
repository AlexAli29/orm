package migrate

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 audit: unique equality has one authority, and both callers reach it.
//
// This is the object next to the index, with the same defect and in the same two
// packages. The migration planner decided whether to rewrite a unique constraint;
// the schema comparison decided whether to report drift, whether the declarations
// and the migrations agree, and whether migrate --fake-applied refuses. Each had
// its own implementation, they agreed, and nothing kept them agreeing.
//
// Unlike index equality it was never observably wrong, and it is not reachable
// from a materialized view at all — a materialized view has no Uniques field. That
// is why it was deferred rather than fixed with the index rule. It is fixed now for
// the same reason the index rule was: a duplicate that agrees today is a duplicate
// that goes quiet in one half tomorrow, and the half that goes quiet is invisible.
//
// The axes are the four that decide identity. The name is not one of them: callers
// pair by name before asking, and a renamed constraint is a different question from
// a changed one.

// uniqueAxes returns a baseline unique object and, for each axis, a variant
// differing from it in exactly that field.
func uniqueAxes() (schema.Unique, map[string]schema.Unique) {
	base := schema.Unique{
		Name:             "users_email_key",
		Columns:          []string{"email", "tenant_id"},
		Constraint:       true,
		Where:            "",
		NullsNotDistinct: true,
	}
	with := func(f func(*schema.Unique)) schema.Unique {
		out := base
		out.Columns = append([]string{}, base.Columns...)
		f(&out)
		return out
	}
	return base, map[string]schema.Unique{
		// The key columns as a sequence: (a, b) and (b, a) are different
		// objects, and only the first serves a lookup on a alone.
		"column order": with(func(u *schema.Unique) {
			u.Columns[0], u.Columns[1] = u.Columns[1], u.Columns[0]
		}),
		"column count": with(func(u *schema.Unique) { u.Columns = u.Columns[:1] }),
		"column name":  with(func(u *schema.Unique) { u.Columns[0] = "other" }),
		// A constraint can be the target of a foreign key and a bare unique
		// index cannot, so the two are not interchangeable.
		"constraint rather than index": with(func(u *schema.Unique) { u.Constraint = false }),
		// A partial unique index proves nothing about the rows it excludes.
		"a predicate appears": with(func(u *schema.Unique) { u.Where = "active" }),
		// PostgreSQL 15's NULLS NOT DISTINCT decides whether two NULLs collide.
		"nulls not distinct": with(func(u *schema.Unique) { u.NullsNotDistinct = false }),
	}
}

// tableWithUnique lends a unique object to a table so the drift comparison can be
// asked about it through the entry point it really uses.
func tableWithUnique(u schema.Unique) *schema.Schema {
	return &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "users",
		Columns: []schema.Column{
			{Name: "email", Type: schema.Type{Name: "text"}},
			{Name: "tenant_id", Type: schema.Type{Name: "int8"}},
			{Name: "other", Type: schema.Type{Name: "text"}},
			{Name: "active", Type: schema.Type{Name: "bool"}},
		},
		Uniques: []schema.Unique{u},
	}}}
}

func driftSaysSameUnique(a, b schema.Unique) bool {
	return len(schema.Diff(tableWithUnique(a), tableWithUnique(b))) == 0
}

// plannerRewritesUnique asks the real table planner whether it would change the
// object, rather than asking a comparison function directly.
func plannerRewritesUnique(have, want schema.Unique) bool {
	d := &Diff{}
	diffUniques(
		schema.Table{Schema: "public", Name: "users", Uniques: []schema.Unique{have}},
		schema.Table{Schema: "public", Name: "users", Uniques: []schema.Unique{want}},
		d)
	return len(d.Operations) > 0
}

// The planner's answer and the drift check's answer are the same on every axis.
func TestUniqueComparison_thePlannerAndTheDriftCheckAgree(t *testing.T) {
	base, variants := uniqueAxes()

	if !sameUnique(base, base) {
		t.Fatal("the planner's comparison says a unique object differs from itself")
	}
	if !driftSaysSameUnique(base, base) {
		t.Fatal("the drift check says a unique object differs from itself")
	}

	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			planner := sameUnique(base, other)
			drift := driftSaysSameUnique(base, other)
			if planner != drift {
				t.Errorf("the two comparisons disagree about a difference in %s: the planner "+
					"says same=%t and the drift check says same=%t. Unique equality is "+
					"supposed to have one authority, and a caller answering it from its own "+
					"field comparison makes exactly one half of the project stop noticing",
					axis, planner, drift)
			}
			if planner {
				t.Errorf("both comparisons call two unique objects differing in %s the same object",
					axis)
			}
		})
	}
}

// The planner reaches the canonical rule rather than answering for itself.
func TestUniqueComparison_thePlannerRoutesThroughTheCanonicalRule(t *testing.T) {
	base, variants := uniqueAxes()
	if plannerRewritesUnique(base, base) {
		t.Fatal("the planner rewrites a unique object that did not change, so every case " +
			"below would pass for the wrong reason")
	}
	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			if !plannerRewritesUnique(base, other) {
				t.Errorf("the planner planned nothing for a unique object differing in %s, so "+
					"a declared change is never migrated", axis)
			}
		})
	}
}

// And the drift check reaches it too, through schema.Diff.
func TestUniqueComparison_theDriftCheckRoutesThroughTheCanonicalRule(t *testing.T) {
	base, variants := uniqueAxes()
	if !driftSaysSameUnique(base, base) {
		t.Fatal("the drift check reports a unique object that did not change")
	}
	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			if driftSaysSameUnique(base, other) {
				t.Errorf("the drift check reported nothing for a unique object differing in %s, "+
					"so a hand-edited constraint is never reported", axis)
			}
		})
	}
}

// A materialized view has no unique objects, which is why this rule is not
// reachable from the G2 surface.
//
// Recorded here rather than left implicit: the reason unique equality was deferred
// past the index work is that no materialized-view workflow can observe it, and if
// that ever stops being true the deferral was wrong and this test says so.
func TestUniqueComparison_isNotReachableFromAMaterializedView(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "totals",
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
		Indexes:    []schema.Index{{Name: "totals_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}},
	}
	// The lent table a materialized view's index comparison uses carries indexes
	// and nothing else, so unique equality is never consulted for one.
	lent := schema.Table{Schema: m.Schema, Name: m.Name, Indexes: m.Indexes}
	lent.Normalize()
	if len(lent.Uniques) != 0 {
		t.Errorf("normalising a materialized view's lent table produced %d unique object(s); "+
			"unique equality is now reachable from the G2 surface and the deferral that "+
			"put this rule after the index rule no longer holds", len(lent.Uniques))
	}
}

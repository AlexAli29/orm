package migrate

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G2: index equality has one authority, and both adapters reach it.
//
// A migration is planned by comparing declared indexes against the state the
// migrations describe; drift is reported by comparing declared indexes against
// the database. Those are different questions asked on different occasions, and
// each used to have its own implementation of "are these two indexes the same
// object" — this package's sameIndex, and schema's.
//
// They agreed. Nothing kept them agreeing. A mutation campaign found it the way
// it found the duplicated eligibility rule: removing GiST comparison from
// schema's copy left every planner test green, so a project's drift check would
// stop reporting a hand-edited GiST index while makemigrations carried on
// planning correctly. The failure is silent in exactly one direction, which is
// the worst shape for it — whichever half went quiet, the other keeps behaving.
//
// The rule now lives in indexcmp with one adapter into it. These tests are what
// stop it from being restated: the first proves the two callers still give the
// same answer on every axis, and the two after it drive the real planner and the
// real drift comparison over the same matrix, so an adapter that answered from
// its own field comparison would keep working while the canonical rule changed
// underneath it — and would be caught here rather than by a user.

// indexAxes returns a baseline index and, for each axis, a variant differing
// from it in exactly that field.
//
// One axis at a time, for the reason the drift tests are: a variant differing in
// two fields would still compare unequal after one of the two comparisons was
// removed, and the disagreement would not show.
func indexAxes() (schema.Index, map[string]schema.Index) {
	base := schema.Index{
		Name:    "ix",
		Unique:  true,
		Method:  "btree",
		Where:   "score > 0",
		Include: []string{"title"},
		Columns: []schema.IndexColumn{
			{Name: "owner_id", Direction: schema.Asc, Nulls: schema.NullsLast, OpClass: "int8_ops"},
			{Name: "score", Direction: schema.Desc, Nulls: schema.NullsFirst},
		},
	}
	with := func(f func(*schema.Index)) schema.Index {
		out := base
		out.Columns = append([]schema.IndexColumn{}, base.Columns...)
		out.Include = append([]string{}, base.Include...)
		f(&out)
		return out
	}
	return base, map[string]schema.Index{
		"uniqueness":   with(func(i *schema.Index) { i.Unique = false }),
		"method hash":  with(func(i *schema.Index) { i.Method = "hash" }),
		"method gist":  with(func(i *schema.Index) { i.Method = "gist" }),
		"method gin":   with(func(i *schema.Index) { i.Method = "gin" }),
		"method brin":  with(func(i *schema.Index) { i.Method = "brin" }),
		"predicate":    with(func(i *schema.Index) { i.Where = "score > 1" }),
		"no predicate": with(func(i *schema.Index) { i.Where = "" }),
		"INCLUDE":      with(func(i *schema.Index) { i.Include = []string{"title", "body"} }),
		"no INCLUDE":   with(func(i *schema.Index) { i.Include = nil }),
		"key name":     with(func(i *schema.Index) { i.Columns[0].Name = "other" }),
		"key count": with(func(i *schema.Index) {
			i.Columns = append(i.Columns, schema.IndexColumn{Name: "extra"})
		}),
		"key order": with(func(i *schema.Index) {
			i.Columns[0], i.Columns[1] = i.Columns[1], i.Columns[0]
		}),
		"direction":      with(func(i *schema.Index) { i.Columns[0].Direction = schema.Desc }),
		"nulls ordering": with(func(i *schema.Index) { i.Columns[0].Nulls = schema.NullsFirst }),
		"operator class": with(func(i *schema.Index) { i.Columns[0].OpClass = "int8_pattern_ops" }),
		"expression": with(func(i *schema.Index) {
			i.Columns[0] = schema.IndexColumn{Expression: "lower(title)"}
		}),
	}
}

// relationWith lends an index to a table so the drift comparison can be asked
// about it through the entry point it really uses.
func relationWith(ix schema.Index) *schema.Schema {
	return &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "t",
		Columns: []schema.Column{
			{Name: "owner_id", Type: schema.Type{Name: "int8"}},
			{Name: "score", Type: schema.Type{Name: "int8"}},
			{Name: "title", Type: schema.Type{Name: "text"}},
			{Name: "body", Type: schema.Type{Name: "text"}},
			{Name: "extra", Type: schema.Type{Name: "text"}},
		},
		Indexes: []schema.Index{ix},
	}}}
}

// driftSaysSame asks schema's comparison through the entry point drift
// detection actually uses.
func driftSaysSame(a, b schema.Index) bool {
	return len(schema.Diff(relationWith(a), relationWith(b))) == 0
}

// plannerPlansAChange asks the real table planner whether it would rewrite the
// index, rather than asking a comparison function directly.
func plannerPlansAChange(have, want schema.Index) bool {
	d := &Diff{}
	diffIndexes(
		schema.Table{Schema: "public", Name: "t", Indexes: []schema.Index{have}},
		schema.Table{Schema: "public", Name: "t", Indexes: []schema.Index{want}},
		d)
	return len(d.Operations) > 0
}

// matViewPlannerPlansAChange asks the same of the materialized-view planner,
// which is a second call site and could route somewhere else.
func matViewPlannerPlansAChange(have, want schema.Index) bool {
	d := &Diff{}
	diffMaterializedViewIndexes(
		schema.MaterializedView{Schema: "public", Name: "m", Indexes: []schema.Index{have}},
		schema.MaterializedView{Schema: "public", Name: "m", Indexes: []schema.Index{want}},
		d)
	return len(d.Operations) > 0
}

// The planner's answer and the drift check's answer are the same on every axis.
func TestIndexComparison_thePlannerAndTheDriftCheckAgree(t *testing.T) {
	base, variants := indexAxes()

	if !sameIndex(base, base) {
		t.Fatal("the planner's comparison says an index differs from itself")
	}
	if !driftSaysSame(base, base) {
		t.Fatal("the drift check says an index differs from itself")
	}

	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			planner := sameIndex(base, other)
			drift := driftSaysSame(base, other)
			if planner != drift {
				t.Errorf("the two comparisons disagree about a difference in %s: the planner "+
					"says same=%t and the drift check says same=%t. Index equality is "+
					"supposed to have one authority, and a caller that answers it from its "+
					"own field comparison makes exactly one half of the project stop "+
					"noticing, silently — either a user is never told about an index "+
					"somebody changed by hand, or never offered the migration for one they "+
					"declared", axis, planner, drift)
			}
			if planner {
				t.Errorf("both comparisons call two indexes differing in %s the same object", axis)
			}
		})
	}

	// An unset method means btree, in both, so that a declaration naming no
	// method and a catalog reporting btree do not differ forever.
	unset, explicit := base, base
	unset.Method = ""
	explicit.Method = "btree"
	if !sameIndex(unset, explicit) || !driftSaysSame(unset, explicit) {
		t.Errorf("an unset method and an explicit btree differ: planner same=%t drift same=%t",
			sameIndex(unset, explicit), driftSaysSame(unset, explicit))
	}
}

// The planner reaches the canonical rule rather than answering for itself.
//
// This drives diffIndexes and diffMaterializedViewIndexes — the two places a
// migration is decided — over the whole axis matrix. A planner that restated
// equality would keep passing the agreement test above only for as long as its
// restatement happened to match; this one goes red the moment the canonical rule
// and the planner's behaviour part company, because the planner's behaviour is
// what it measures.
func TestIndexComparison_thePlannerRoutesThroughTheCanonicalRule(t *testing.T) {
	base, variants := indexAxes()

	if plannerPlansAChange(base, base) {
		t.Fatal("the planner rewrites an index that did not change, so every case below " +
			"would pass for the wrong reason")
	}
	if matViewPlannerPlansAChange(base, base) {
		t.Fatal("the materialized-view planner rewrites an index that did not change")
	}

	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			if !plannerPlansAChange(base, other) {
				t.Errorf("the table planner planned nothing for an index differing in %s, so "+
					"a declared change is never migrated and the database keeps an index "+
					"the declarations do not describe", axis)
			}
			if !matViewPlannerPlansAChange(base, other) {
				t.Errorf("the materialized-view planner planned nothing for an index "+
					"differing in %s", axis)
			}
		})
	}
}

// And the drift check reaches it too, through schema.Diff.
//
// schema.Diff is what orm check reports live index drift from, what the managed
// comparison reports as model changes between the migrations and the
// declarations, and what migrate --fake-applied refuses on. One rule decides all
// three, and this is the assertion that it is reached.
func TestIndexComparison_theDriftCheckRoutesThroughTheCanonicalRule(t *testing.T) {
	base, variants := indexAxes()

	if !driftSaysSame(base, base) {
		t.Fatal("the drift check reports an index that did not change")
	}
	for axis, other := range variants {
		t.Run(axis, func(t *testing.T) {
			if driftSaysSame(base, other) {
				t.Errorf("the drift check reported nothing for an index differing in %s, so "+
					"a hand-edited index is never reported and the declaration and the "+
					"database disagree permanently", axis)
			}
		})
	}
}

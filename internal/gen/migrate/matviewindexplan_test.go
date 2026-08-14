package migrate

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G2 #4: what the planner does about each index difference, one at a time.
//
// Planning an index change has three outcomes, and they are reached by different
// code: an index only in the declarations is created, one only in the state is
// dropped, and one present in both but different is dropped and created again
// because PostgreSQL cannot alter an index in place.
//
// The drop branch is the one worth isolating. It is what makes a project
// converge: a state entry the declarations do not have must produce a migration
// that removes it, and the whole visible symptom of duplicated index state was
// that the next plan kept finding an entry nobody declared. Remove that branch
// and the project converges immediately and permanently — every plan is empty,
// which reads exactly like agreement. Nothing about the database is wrong, so no
// catalog assertion finds it; what is wrong is that the state and the
// declarations disagree forever and the tool says they do not.
//
// The end-to-end convergence fixture catches it. It cannot say which branch went
// missing, so the branches are asserted here directly.

// matWith returns a materialized view carrying the given indexes.
func matWith(indexes ...schema.Index) schema.MaterializedView {
	return schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id AS user_id FROM users"},
		Indexes:    indexes,
	}
}

func planIndexes(have, want schema.MaterializedView) []Operation {
	d := &Diff{}
	diffMaterializedViewIndexes(have, want, d)
	return d.Operations
}

func TestMatViewIndexPlan_eachDifferenceHasItsOwnOutcome(t *testing.T) {
	declared := schema.Index{Name: "totals_user_id_idx",
		Columns: []schema.IndexColumn{{Name: "user_id"}}}
	unique := declared
	unique.Unique = true

	// Agreement plans nothing, or every case below would also be satisfied by a
	// planner that rewrote the indexes on every run.
	if got := planIndexes(matWith(declared), matWith(declared)); len(got) != 0 {
		t.Errorf("an unchanged index planned %d operation(s): %v", len(got), describeAll(got))
	}

	t.Run("only in the declarations", func(t *testing.T) {
		got := planIndexes(matWith(), matWith(declared))
		if len(got) != 1 {
			t.Fatalf("planned %v, want one create", describeAll(got))
		}
		if _, ok := got[0].(CreateIndex); !ok {
			t.Errorf("planned %s, want a create", got[0].Describe())
		}
	})

	t.Run("only in the state", func(t *testing.T) {
		got := planIndexes(matWith(declared), matWith())
		if len(got) != 1 {
			t.Fatalf("planned %v, want one drop. An index the declarations do not have must "+
				"be removed, or the state and the declarations disagree forever while "+
				"every plan comes out empty — which is what a converged project looks "+
				"like from outside", describeAll(got))
		}
		drop, ok := got[0].(DropIndex)
		if !ok {
			t.Fatalf("planned %s, want a drop", got[0].Describe())
		}
		if drop.Name != declared.Name {
			t.Errorf("dropped %s, want %s", drop.Name, declared.Name)
		}
	})

	t.Run("in both and different", func(t *testing.T) {
		got := planIndexes(matWith(declared), matWith(unique))
		if len(got) != 2 {
			t.Fatalf("planned %v, want a drop and a create", describeAll(got))
		}
		if _, ok := got[0].(DropIndex); !ok {
			t.Errorf("the first operation is %s, want the drop first so the name is free",
				got[0].Describe())
		}
		if _, ok := got[1].(CreateIndex); !ok {
			t.Errorf("the second operation is %s, want a create", got[1].Describe())
		}
	})

	// And the policy about a duplicated state entry, recorded as it actually is
	// rather than as a test might wish it were.
	//
	// Both loops match by name, so a state holding one index twice against one
	// declaration plans nothing: the create loop finds a match and the drop loop
	// finds every copy's name declared. The planner cannot repair a duplicate and
	// does not try.
	//
	// That is deliberate, and it is why the classes upstream of here matter so
	// much. A migration is planned against the state the migrations describe, and
	// the planner never emits a create for an index already in that state — so
	// the only way a duplicate can arise is an operation that adds one when
	// replayed. That is exactly the defect CreateMaterializedView had when it
	// carried its indexes in its payload alongside the CreateIndex beside it, and
	// it is what the replay chain tests pin down. Correctness here depends on the
	// duplicate never being created, not on it being detected afterwards; a
	// reader who assumed otherwise would be looking for a repair that is not
	// there.
	t.Run("a duplicated state entry is not repaired here", func(t *testing.T) {
		got := planIndexes(matWith(declared, declared), matWith(declared))
		if len(got) != 0 {
			t.Errorf("the planner now plans %v for a duplicated state entry. That may be an "+
				"improvement, but it is a change of policy: convergence used to depend "+
				"entirely on replay never producing a duplicate, and this test recorded "+
				"that it did", describeAll(got))
		}
	})
}

func describeAll(ops []Operation) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.Describe())
	}
	return out
}

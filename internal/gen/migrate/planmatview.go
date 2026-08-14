package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Planning materialized-view migrations.
//
// The policy here is narrower than the one for ordinary views, and deliberately
// so. An ordinary view whose body changes can be replaced in place: PostgreSQL
// keeps the grants, the ownership and the dependents, and the only thing that
// moves is the SELECT. A materialized view has no such operation. Changing what
// one selects means dropping it and creating it again, which discards the
// stored rows, the indexes built on them, the grants and every dependent
// object — and none of that is visible in a diff of two SELECTs.
//
// So this planner creates and it drops, and for anything in between it refuses.
// The refusals are not gaps waiting to be filled in: they are the milestone's
// position on what a generated migration is allowed to decide by itself.

// diffMaterializedViews plans the materialized-view half of a migration.
//
// Ordering is the caller's: creations arrive here already sequenced against the
// views and materialized views they read, because a relation cannot be created
// before what it selects from and the graph does not care which kind each node
// is.
func planMaterializedViewCreate(state, desired *schema.Schema, in ViewPlanInput, m schema.MaterializedView, d *Diff) error {
	have, existed := findMaterializedView(state.MaterializedViews, m.Qualified())
	if !existed {
		if err := refuseOccupiedNameForMatView(state, in, m); err != nil {
			return err
		}
		// The relation is created without its indexes, and each index follows
		// as its own operation. Both halves matter.
		//
		// Indexes come after the relation they are built on, always: there is
		// no ordering in which CREATE INDEX precedes the CREATE it indexes.
		//
		// And the create operation must not also carry them, or replaying the
		// migration adds every index twice — once from the relation it
		// describes and once from the CreateIndex that follows. The duplicate
		// is invisible in the database, which only ever saw one CREATE INDEX,
		// and visible in the migration state, where the next plan finds an
		// index the declarations do not have and writes a migration to drop
		// it. That migration never converges: it re-plans the same drop and
		// create on every run.
		bare := m.Clone()
		bare.Indexes = nil
		d.Operations = append(d.Operations, CreateMaterializedView{View: bare})
		for _, ix := range m.Indexes {
			d.Operations = append(d.Operations, CreateIndex{Schema: m.Schema, Table: m.Name, Index: ix})
		}
		return nil
	}

	// The relation exists and this project created it. Two things can have
	// moved: the definition, which is refused, and the indexes, which are not.
	if err := refuseDefinitionChange(in, have, m); err != nil {
		return err
	}
	if err := refuseCreationPolicyChange(have, m); err != nil {
		return err
	}
	diffMaterializedViewIndexes(have, m, d)
	return nil
}

// refuseOccupiedNameForMatView stops a creation whose name something else holds.
//
// The three cases are separated because they need different sentences: another
// kind of relation in the state, another kind in the database, and a
// materialized view of this name that no migration of this project created.
func refuseOccupiedNameForMatView(state *schema.Schema, in ViewPlanInput, m schema.MaterializedView) error {
	if kind, ok := state.Relation(m.Schema, m.Name); ok && kind != schema.KindMaterializedView {
		return fmt.Errorf("cannot create materialized view %s: the migration state already has a "+
			"%s of that name. PostgreSQL has one namespace for relations, so creating this means "+
			"removing that one — which is a decision a migration should state rather than infer",
			m.Qualified(), kind)
	}
	if in.Actual == nil {
		return nil
	}
	if kind, ok := in.Actual.Relation(m.Schema, m.Name); ok && kind != schema.KindMaterializedView {
		return fmt.Errorf("cannot create materialized view %s: the database already has a %s of "+
			"that name. Dropping it is not something this planner will decide on its own; write "+
			"the migration that says what happens to it", m.Qualified(), kind)
	}
	if _, ok := in.Actual.Relation(m.Schema, m.Name); ok {
		return fmt.Errorf("cannot create materialized view %s: one of that name already exists in "+
			"the database and no migration of this project created it.\n\nAdopting it would mean "+
			"treating a definition nothing here has read as the declared one, and recreating it "+
			"would discard rows this project never computed. Write a migration that says what "+
			"happens to it, or remove it by hand first", m.Qualified())
	}
	return nil
}

// refuseDefinitionChange is the central invariant of G2.
//
// A materialized view whose declared definition has moved is refused, and the
// refusal does not weaken because the output shape happens to match. Identical
// columns and identical types say nothing about the rows: WHERE active and
// WHERE active AND verified produce the same shape and different data, and
// recreating to pick up the second discards whatever the first computed.
//
// It also refuses when the database was never consulted, and when the database
// no longer holds what this project applied — for the same reason the ordinary
// view planner does. Something has to have read the body before a plan can
// claim to know what is there.
func refuseDefinitionChange(in ViewPlanInput, have, want schema.MaterializedView) error {
	sameSource := have.Definition.Identity() == want.Definition.Identity()

	if !in.Online {
		if sameSource {
			// Nothing declared about it moved, and nothing is being written, so
			// there is nothing an offline plan could get wrong.
			return nil
		}
		return fmt.Errorf("cannot migrate materialized view %s: its definition changed and no "+
			"database was consulted.\n\n%s", want.Qualified(), definitionChangeAdvice(want))
	}

	// The database must still hold what this project applied, whether or not
	// the declaration moved: a manual edit is not something a later migration
	// should quietly paper over.
	recorded, known := in.Recorded[want.Qualified()]
	if !known {
		return fmt.Errorf("cannot migrate materialized view %s: no migration of this project "+
			"recorded what it applied there, so there is nothing to compare the database "+
			"against.\n\nThe columns and the kind can match while the body differs entirely, and "+
			"a materialized view holds rows that were computed from that body. Write an explicit "+
			"migration, or remove it by hand first", want.Qualified())
	}
	if in.Actual != nil {
		if actual, ok := findMaterializedView(in.Actual.MaterializedViews, want.Qualified()); ok {
			if !schema.SameOnServer(recorded, actual.Definition.Canonical) {
				return fmt.Errorf("cannot migrate materialized view %s: the database no longer "+
					"holds the definition this project applied to it. Somebody changed it outside "+
					"the migrations.\n\nA change to the declaration is not permission to erase "+
					"that, and recreating would discard the rows currently stored. Reconcile the "+
					"two first — either restore the view, or bring the declaration in line with "+
					"what is actually there", want.Qualified())
			}
		}
	}

	if sameSource {
		return nil
	}
	return fmt.Errorf("cannot migrate materialized view %s: its definition changed.\n\n%s",
		want.Qualified(), definitionChangeAdvice(want))
}

// definitionChangeAdvice is the sentence that says why this is refused and what
// to do instead. It is one string because the two callers must not drift apart.
func definitionChangeAdvice(m schema.MaterializedView) string {
	return "PostgreSQL has no CREATE OR REPLACE MATERIALIZED VIEW, so the only way to change what " +
		"one selects is to drop it and create it again — which discards the stored rows, the " +
		"indexes built on them, the grants and every dependent object. This planner will not " +
		"decide that on its own.\n\nWrite an explicit migration that drops and recreates " +
		m.Qualified() + " if that is what you mean. Note that a change to whitespace or comments " +
		"also changes the declaration's identity: what is compared is the source this project " +
		"states, not a normalised form of it"
}

// refuseCreationPolicyChange refuses a WITH DATA / WITH NO DATA change on a
// relation that already exists.
//
// WITH DATA is creation policy: it decides whether CREATE populates the view,
// and it has no meaning afterwards. Changing the declaration cannot be honoured
// without recreating the relation, and reading it as an instruction to REFRESH
// — or to REFRESH WITH NO DATA, which discards every row — would be inventing a
// meaning the declaration does not have.
func refuseCreationPolicyChange(have, want schema.MaterializedView) error {
	if have.WithData == want.WithData {
		return nil
	}
	from, to := "WITH DATA", "WITH NO DATA"
	if want.WithData {
		from, to = "WITH NO DATA", "WITH DATA"
	}
	return fmt.Errorf("cannot migrate materialized view %s: its creation policy changed from %s "+
		"to %s, and that only applies when the relation is created.\n\nThe relation already "+
		"exists, so there is nothing for the new policy to do. It is not read as an instruction "+
		"to refresh: REFRESH WITH NO DATA discards every row, and no declaration should mean that "+
		"by implication. Refresh it yourself, or write an explicit migration that recreates it",
		want.Qualified(), from, to)
}

// diffMaterializedViewIndexes plans index changes on a materialized view that
// is otherwise unchanged.
//
// It is the ordinary index differ, over the ordinary index model, producing the
// ordinary operations. There is no materialized-view index type and no reduced
// renderer: an index on a materialized view is an index, CREATE INDEX ... ON
// takes the same relation name whichever kind it is, and a unique one over
// plain columns is exactly what REFRESH CONCURRENTLY needs. Changing one never
// touches the relation itself.
func diffMaterializedViewIndexes(have, want schema.MaterializedView, d *Diff) {
	for _, w := range want.Indexes {
		i := slices.IndexFunc(have.Indexes, func(x schema.Index) bool { return x.Name == w.Name })
		if i < 0 {
			d.Operations = append(d.Operations, CreateIndex{Schema: want.Schema, Table: want.Name, Index: w})
			continue
		}
		if sameIndex(have.Indexes[i], w) {
			continue
		}
		// An index cannot be altered in place. Dropping first keeps the name
		// free for the replacement, and neither statement touches the rows.
		d.Operations = append(d.Operations,
			DropIndex{Schema: have.Schema, Table: have.Name, Name: w.Name, Concurrently: w.Concurrently},
			CreateIndex{Schema: want.Schema, Table: want.Name, Index: w})
	}
	for _, h := range have.Indexes {
		if !slices.ContainsFunc(want.Indexes, func(x schema.Index) bool { return x.Name == h.Name }) {
			d.Operations = append(d.Operations,
				DropIndex{Schema: have.Schema, Table: have.Name, Name: h.Name, Concurrently: h.Concurrently})
		}
	}
}

// refuseUnknownMatViewDependents stops a drop that would strand something.
//
// Same contract as the ordinary view planner: PostgreSQL would refuse the drop,
// and CASCADE would remove the dependents without anybody having listed them.
func refuseUnknownMatViewDependents(in ViewPlanInput, m schema.MaterializedView, dropping map[string]bool) error {
	if in.Actual == nil {
		return nil
	}
	var dependents []string
	for _, other := range in.Actual.Views {
		if dropping[other.Qualified()] {
			continue
		}
		for _, dep := range other.DependsOn {
			if dep.Qualified() == m.Qualified() {
				dependents = append(dependents, other.Qualified())
			}
		}
	}
	for _, other := range in.Actual.MaterializedViews {
		if other.Qualified() == m.Qualified() || dropping[other.Qualified()] {
			continue
		}
		for _, dep := range other.DependsOn {
			if dep.Qualified() == m.Qualified() {
				dependents = append(dependents, other.Qualified())
			}
		}
	}
	slices.Sort(dependents)
	dependents = slices.Compact(dependents)
	if len(dependents) == 0 {
		return nil
	}
	return fmt.Errorf("cannot drop materialized view %s: %s still depends on it, and this project "+
		"does not declare them.\n\nPostgreSQL would refuse the drop, and CASCADE would remove them "+
		"without anybody having listed them. Drop them first, or bring them under management",
		m.Qualified(), strings.Join(dependents, ", "))
}

// findMaterializedView looks one up by qualified name.
func findMaterializedView(ms []schema.MaterializedView, qualified string) (schema.MaterializedView, bool) {
	for _, m := range ms {
		if m.Qualified() == qualified {
			return m, true
		}
	}
	return schema.MaterializedView{}, false
}

package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Planning view migrations.
//
// The planner does not ask whether it can produce SQL. It asks whether it can
// prove the transition safe under what this project models, and refuses when it
// cannot. Those are different questions, and the second is the one that keeps a
// migration from being discovered wrong during a deployment.
//
// Four things make it refuse, and each is a case where a plausible-looking
// statement would do damage:
//
//   - a name occupied by another kind of relation, where creating means
//     dropping something that was never declared;
//   - an existing view whose body this project never applied, where replacing
//     means overwriting a definition nothing has read;
//   - a database whose view has drifted since it was applied, where a source
//     change would silently erase somebody's manual fix;
//   - an output shape PostgreSQL will not accept a replacement for, where the
//     alternative is a drop and a create that loses grants and dependents.

// ViewPlanInput is what planning views needs beyond the migration state.
type ViewPlanInput struct {
	// Actual is the live database, when one is reachable. It is nil for an
	// offline plan, and its absence is what the provenance and drift checks
	// report on rather than silently skipping.
	Actual *schema.Schema
	// Recorded maps a qualified name to the canonical definition this project
	// last applied there, read from the per-database recording.
	Recorded map[string]string
	// Online says whether a database was consulted at all.
	Online bool
}

// planViewCreate plans one ordinary view: create it, replace it, or leave it.
//
// The ordering that used to live here moved to planStoredRelations, because a
// view can select from a materialized view and the two kinds have to be
// sequenced in one graph rather than two. What each kind is allowed to do is
// unchanged, and this is still the only place that decides it for a view.
func planViewCreate(state, desired *schema.Schema, in ViewPlanInput, v schema.View, d *Diff) error {
	var old schema.View
	existed := false
	for _, s := range state.Views {
		if s.Qualified() == v.Qualified() {
			old, existed = s, true
			break
		}
	}
	if !existed {
		if err := refuseOccupiedName(state, desired, in, v); err != nil {
			return err
		}
		d.Operations = append(d.Operations, CreateView{View: v})
		return nil
	}
	if old.Definition.Identity() == v.Definition.Identity() && sameColumns(old, v) {
		return nil // Nothing declared about it moved.
	}
	if err := refuseUnsafeReplacement(in, old, v); err != nil {
		return err
	}
	d.Operations = append(d.Operations, ReplaceView{View: v})
	return nil
}

// refuseOccupiedName stops a creation whose name something else already holds.
func refuseOccupiedName(state, desired *schema.Schema, in ViewPlanInput, v schema.View) error {
	if kind, ok := state.Relation(v.Schema, v.Name); ok && kind != schema.KindView {
		return fmt.Errorf("cannot create view %s: the migration state already has a %s of that "+
			"name. PostgreSQL has one namespace for relations, so creating this view means "+
			"removing that one — which is a decision a migration should state rather than infer",
			v.Qualified(), kind)
	}
	if in.Actual != nil {
		if kind, ok := in.Actual.Relation(v.Schema, v.Name); ok && kind != schema.KindView {
			return fmt.Errorf("cannot create view %s: the database already has a %s of that name. "+
				"Dropping it is not something this planner will decide on its own; write the "+
				"migration that says what happens to it", v.Qualified(), kind)
		}
		if _, ok := in.Actual.Relation(v.Schema, v.Name); ok {
			// A view of this name exists but the migration state does not know
			// about it, so this project never applied it.
			return fmt.Errorf("cannot create view %s: a view of that name already exists in the "+
				"database and no migration of this project created it. Its body may differ from "+
				"the declaration in ways no comparison here can see, so adopting it is an explicit "+
				"decision: write a migration that drops and recreates it, or remove it by hand first",
				v.Qualified())
		}
	}
	return nil
}

// refuseUnsafeReplacement stops a replacement that cannot be proved safe.
func refuseUnsafeReplacement(in ViewPlanInput, old, want schema.View) error {
	// PostgreSQL's own rule, proved before anything is written.
	if why := ReplaceEligible(old, want); why != "" {
		return fmt.Errorf("cannot replace view %s: %s.\n\nCREATE OR REPLACE VIEW is the only "+
			"in-place change PostgreSQL offers, and a drop and create is not the same thing: it "+
			"loses grants, ownership and every dependent object, none of which this project "+
			"models. Write the migration explicitly if that is what you mean", want.Qualified(), why)
	}
	if !in.Online {
		// Nothing consulted the database, so neither of the two proofs below
		// was possible. Replacement is refused rather than allowed: the whole
		// point of this planner is that it refuses what it cannot prove, and an
		// unproved replacement overwrites a body nothing has read.
		//
		// Creation is unaffected — an absent relation has nothing to overwrite.
		return fmt.Errorf("cannot replace view %s: no database was consulted, so this planner "+
			"could not check that the definition it is replacing is the one this project applied. "+
			"Replacing on that basis would overwrite whatever is actually there",
			want.Qualified())
	}

	// The database must be holding what this project last applied. Two ways it
	// might not be, and they need different sentences.
	recorded, known := in.Recorded[want.Qualified()]
	if !known {
		return fmt.Errorf("cannot replace view %s: no migration of this project recorded what it "+
			"applied there, so there is nothing to compare the database against. Replacing would "+
			"overwrite a definition nothing has read — the columns and the kind can match while "+
			"the body differs entirely", want.Qualified())
	}
	if in.Actual != nil {
		for _, a := range in.Actual.Views {
			if a.Qualified() != want.Qualified() {
				continue
			}
			if !schema.SameOnServer(recorded, a.Definition.Canonical) {
				return fmt.Errorf("cannot replace view %s: the database no longer holds the "+
					"definition this project applied to it. Somebody changed it outside the "+
					"migrations.\n\nA change to the declaration is not permission to erase that. "+
					"Reconcile the two first — either restore the view, or bring the declaration "+
					"in line with what is actually there", want.Qualified())
			}
		}
	}
	return nil
}

// refuseUnknownDependents stops a drop PostgreSQL would refuse anyway, with a
// sentence rather than an error at apply time.
func refuseUnknownDependents(in ViewPlanInput, v schema.View, dropping map[string]bool) error {
	if in.Actual == nil {
		return nil
	}
	var dependents []string
	for _, other := range in.Actual.Views {
		if dropping[other.Qualified()] {
			continue
		}
		for _, d := range other.DependsOn {
			if d.Qualified() == v.Qualified() {
				dependents = append(dependents, other.Qualified())
			}
		}
	}
	for _, m := range in.Actual.MaterializedViews {
		if dropping[m.Qualified()] {
			continue
		}
		for _, d := range m.DependsOn {
			if d.Qualified() == v.Qualified() {
				dependents = append(dependents, m.Qualified())
			}
		}
	}
	slices.Sort(dependents)
	dependents = slices.Compact(dependents)
	if len(dependents) == 0 {
		return nil
	}
	return fmt.Errorf("cannot drop view %s: %s still depends on it, and this project does not "+
		"declare them.\n\nPostgreSQL would refuse the drop, and CASCADE would remove them without "+
		"anybody having listed them. Drop them first, or bring them under management",
		v.Qualified(), strings.Join(dependents, ", "))
}

// sameColumns reports whether two views declare the same output shape.
func sameColumns(a, b schema.View) bool {
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i].Name != b.Columns[i].Name ||
			a.Columns[i].Type.String() != b.Columns[i].Type.String() {
			return false
		}
	}
	return true
}

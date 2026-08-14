package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Ordering stored queries across kinds.
//
// A view can select from a materialized view and a materialized view can select
// from a view, so the two cannot be ordered separately: sorting the views among
// themselves and then the materialized views among themselves produces a
// sequence that is correct within each group and wrong between them. What
// decides the order is the dependency graph, and the graph does not care which
// kind each node is.
//
// So there is one traversal over one node list, and the kind is only consulted
// when the operation is emitted.

// relationNode is one stored query in the dependency graph.
type relationNode struct {
	qualified string
	dependsOn []schema.RelationRef
	// Exactly one of these is set.
	view    *schema.View
	matview *schema.MaterializedView
}

// planStoredRelations plans views and materialized views together.
//
// Creations run in dependency order and drops in the reverse of it, both over
// the combined graph. The per-kind decisions — what may be replaced, what must
// be refused — are unchanged and live where they always did.
func planStoredRelations(state, desired *schema.Schema, in ViewPlanInput, d *Diff) error {
	ordered, err := topologicalRelations(nodesOf(desired))
	if err != nil {
		return err
	}
	for _, n := range ordered {
		switch {
		case n.view != nil:
			if err := planViewCreate(state, desired, in, *n.view, d); err != nil {
				return err
			}
		case n.matview != nil:
			if err := planMaterializedViewCreate(state, desired, in, *n.matview, d); err != nil {
				return err
			}
		}
	}

	// Drops, in reverse dependency order: a relation is removed before what it
	// reads, so nothing is dropped while something still selects from it and
	// nothing needs CASCADE to succeed.
	gone := &schema.Schema{}
	wantView := map[string]bool{}
	for _, v := range desired.Views {
		wantView[v.Qualified()] = true
	}
	wantMat := map[string]bool{}
	for _, m := range desired.MaterializedViews {
		wantMat[m.Qualified()] = true
	}
	for _, v := range state.Views {
		if !wantView[v.Qualified()] {
			gone.Views = append(gone.Views, v)
		}
	}
	for _, m := range state.MaterializedViews {
		if !wantMat[m.Qualified()] {
			gone.MaterializedViews = append(gone.MaterializedViews, m)
		}
	}

	dropOrder, err := topologicalRelations(nodesOf(gone))
	if err != nil {
		return err
	}
	slices.Reverse(dropOrder)

	// A relation that is itself being dropped is not a dependent that would be
	// stranded: the reversed order removes it first. Counting it would make a
	// whole managed chain impossible to remove — the planner would refuse to
	// drop the middle of it because the end still existed, while the end was
	// two statements above in the same migration.
	dropping := make(map[string]bool, len(dropOrder))
	for _, n := range dropOrder {
		dropping[n.qualified] = true
	}

	for _, n := range dropOrder {
		switch {
		case n.view != nil:
			if err := refuseUnknownDependents(in, *n.view, dropping); err != nil {
				return err
			}
			d.Operations = append(d.Operations, DropView{Schema: n.view.Schema, Name: n.view.Name})
		case n.matview != nil:
			if err := refuseUnknownMatViewDependents(in, *n.matview, dropping); err != nil {
				return err
			}
			// The indexes go with the relation: DROP MATERIALIZED VIEW removes
			// them, and emitting a DropIndex for each first would be writing
			// statements whose only effect is to make the drop's own work
			// visible twice.
			d.Operations = append(d.Operations,
				DropMaterializedView{Schema: n.matview.Schema, Name: n.matview.Name})
		}
	}
	return nil
}

// nodesOf builds the graph's node list from a schema.
func nodesOf(s *schema.Schema) []relationNode {
	nodes := make([]relationNode, 0, len(s.Views)+len(s.MaterializedViews))
	for i := range s.Views {
		v := s.Views[i]
		nodes = append(nodes, relationNode{qualified: v.Qualified(), dependsOn: v.DependsOn, view: &v})
	}
	for i := range s.MaterializedViews {
		m := s.MaterializedViews[i]
		nodes = append(nodes, relationNode{qualified: m.Qualified(), dependsOn: m.DependsOn, matview: &m})
	}
	return nodes
}

// topologicalRelations orders nodes so that every one follows what it reads.
//
// The traversal is over a sorted node list with sorted edges, so one project
// produces one order on every run. A migration whose statement order depended
// on map iteration would be a migration that reviewed differently each time it
// was generated.
//
// A dependency on something outside this set — a table, or a relation the
// project does not declare — is not an edge here. It is still ordered
// correctly, because tables are created before any stored query and an
// undeclared relation already exists.
func topologicalRelations(nodes []relationNode) ([]relationNode, error) {
	byName := make(map[string]relationNode, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		byName[n.qualified] = n
		names = append(names, n.qualified)
	}
	slices.Sort(names)

	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(names))
	out := make([]relationNode, 0, len(nodes))
	var stack []string

	var visit func(string) error
	visit = func(n string) error {
		switch colour[n] {
		case black:
			return nil
		case grey:
			at := slices.Index(stack, n)
			loop := append(slices.Clone(stack[at:]), n)
			return fmt.Errorf("these relations depend on each other: %s. There is no order they "+
				"can be created in, so no migration is written", strings.Join(loop, " -> "))
		}
		colour[n] = grey
		stack = append(stack, n)

		deps := make([]string, 0, len(byName[n].dependsOn))
		for _, dep := range byName[n].dependsOn {
			if _, managed := byName[dep.Qualified()]; managed {
				deps = append(deps, dep.Qualified())
			}
		}
		slices.Sort(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
		out = append(out, byName[n])
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return out, nil
}

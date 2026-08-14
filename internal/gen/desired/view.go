package desired

import (
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Building the desired schema's views.
//
// A view's columns come from the Go declaration the same way a table's do, so
// the column model, the type mapping and the nullability rules are the ones
// already in use. What is different is the definition and the dependencies, and
// both are declared rather than derived: nothing here reads the SQL.

// relationShape builds the columns and indexes a view declaration carries, by
// building the table the same declaration would have described and taking what
// a view is allowed to keep.
//
// Reusing the table builder is deliberate. Columns, tags, type mapping,
// nullability, index and unique declarations are all identical on a view — a
// view column is a column — and a second builder for them would be a second
// place for a covering index or a partial predicate to be quietly dropped.
// What a view may not have is refused here rather than silently discarded.
func (b *builder) relationShape(e *model.GoEntity) (cols []schema.Column, indexes []schema.Index, ok bool) {
	t, ok := b.table(e)
	if !ok {
		return nil, nil, false
	}
	if t.PrimaryKey != nil {
		b.fail("%s: %s declares a primary key, and PostgreSQL allows none on a %s. "+
			"A materialized view's uniqueness is a unique index, which is also what "+
			"REFRESH CONCURRENTLY needs", e.Pos, e.Display(), e.Kind)
		return nil, nil, false
	}
	if len(t.ForeignKeys) > 0 || len(t.Checks) > 0 {
		b.fail("%s: %s declares constraints, and PostgreSQL allows none on a %s",
			e.Pos, e.Display(), e.Kind)
		return nil, nil, false
	}
	indexes = t.Indexes
	// A unique tag on a view column is a unique index rather than a constraint,
	// because PostgreSQL has no unique constraint to create here.
	for _, u := range t.Uniques {
		if u.Constraint {
			b.fail("%s: %s declares a unique constraint, and PostgreSQL allows none on a %s. "+
				"Declare //orm:index ... unique instead", e.Pos, e.Display(), e.Kind)
			return nil, nil, false
		}
	}
	return t.Columns, indexes, true
}

// view builds an ordinary view from its declaration.
func (b *builder) view(e *model.GoEntity) (schema.View, bool) {
	cols, indexes, ok := b.relationShape(e)
	if !ok {
		return schema.View{}, false
	}
	if len(indexes) > 0 {
		b.fail("%s: %s declares an index, and PostgreSQL allows none on an ordinary view. "+
			"An index belongs on a table or on a materialized view, which stores its rows",
			e.Pos, e.Display())
		return schema.View{}, false
	}
	ref := model.TableRef{Schema: b.qualify(e.Table.Schema), Name: e.Table.Name}
	def, ok := b.definition(e)
	if !ok {
		return schema.View{}, false
	}
	return schema.View{
		Schema:     ref.Schema,
		Name:       ref.Name,
		Columns:    cols,
		Definition: def,
		DependsOn:  b.dependencies(e),
	}, true
}

// materializedView builds a materialized view from its declaration.
func (b *builder) materializedView(e *model.GoEntity) (schema.MaterializedView, bool) {
	cols, indexes, ok := b.relationShape(e)
	if !ok {
		return schema.MaterializedView{}, false
	}
	ref := model.TableRef{Schema: b.qualify(e.Table.Schema), Name: e.Table.Name}
	def, ok := b.definition(e)
	if !ok {
		return schema.MaterializedView{}, false
	}
	m := schema.MaterializedView{
		Schema:     ref.Schema,
		Name:       ref.Name,
		Columns:    cols,
		Definition: def,
		DependsOn:  b.dependencies(e),
		// WITH DATA is PostgreSQL's own default and is what somebody declaring
		// a materialized view almost always means: a view created empty is
		// unreadable until something refreshes it, which is a surprise nobody
		// asked for by saying nothing. //orm:with-no-data opts out.
		WithData: e.View == nil || !e.View.WithNoData,
	}
	m.Indexes = indexes
	return m, true
}

// definition returns the declared definition, refusing a view that has none.
//
// A view without a definition is not a view: there is nothing to create. This
// is reported here, before anything plans a migration, because the alternative
// is discovering it while rendering DDL — by which point the message is about
// an empty string rather than about a missing declaration.
func (b *builder) definition(e *model.GoEntity) (schema.Definition, bool) {
	if e.View == nil || e.View.Mode == model.DefinitionNone || strings.TrimSpace(e.View.SQL) == "" {
		b.failCode(diag.E025, "%s: %s declares %s %s with no definition. Add "+
			"//orm:definition with the SELECT it stands for, either as quoted SQL or as a "+
			"path to a .sql file beside the declaration",
			e.Pos, e.Display(), e.Kind, b.qualify(e.Table.Schema)+"."+e.Table.Name)
		return schema.Definition{}, false
	}
	// Only the project's own text is known here. Canonical is filled in from
	// pg_get_viewdef once the relation exists on a server; until then there is
	// nothing to canonicalise against, and inventing one would mean
	// normalising SQL in Go.
	return schema.Definition{SQL: strings.TrimSpace(e.View.SQL)}, true
}

// dependencies returns the declared dependencies, in a deterministic order.
func (b *builder) dependencies(e *model.GoEntity) []schema.RelationRef {
	if e.View == nil {
		return nil
	}
	out := make([]schema.RelationRef, 0, len(e.View.DependsOn))
	for _, d := range e.View.DependsOn {
		out = append(out, schema.RelationRef{Schema: b.qualify(d.Schema), Name: d.Name})
	}
	schema.SortRefs(out)
	return slices.CompactFunc(out, func(a, c schema.RelationRef) bool {
		return a.Schema == c.Schema && a.Name == c.Name
	})
}

// checkDependencies validates every declared dependency against the schema the
// declarations actually build.
//
// Dependencies are authoritative for managed ordering, which is exactly why
// they are checked: a name nobody declares would order a view after nothing,
// and a view that named itself would order it after itself. Both are silent
// until a migration runs in the wrong order.
func (b *builder) checkDependencies(s *schema.Schema) {
	var nodes []depNode
	add := func(qualified string, deps []schema.RelationRef, pos model.Position, who string) {
		n := depNode{name: qualified}
		for _, d := range deps {
			target := d.Qualified()
			if target == qualified {
				b.failCode(diag.E028, "%s: %s depends on itself. A relation cannot be created "+
					"after itself, so there is no order this could be applied in", pos, who)
				continue
			}
			if _, exists := s.Relation(d.Schema, d.Name); !exists {
				b.failCode(diag.E027, "%s: %s depends on %s, which no declaration in this project "+
					"describes. A dependency orders migrations, so it has to name something this "+
					"project creates — check the schema qualification, or remove the dependency if "+
					"the relation is managed elsewhere", pos, who, target)
				continue
			}
			n.deps = append(n.deps, target)
		}
		nodes = append(nodes, n)
	}

	// The position comes from the declaration that produced the relation, so a
	// diagnostic points at the line somebody wrote rather than at the schema.
	pos := make(map[string]model.Position, len(b.entities))
	for _, e := range b.entities {
		if e.Kind != model.RelTable {
			pos[b.qualify(e.Table.Schema)+"."+e.Table.Name] = e.Pos
		}
	}
	for _, v := range s.Views {
		add(v.Qualified(), v.DependsOn, pos[v.Qualified()], v.Qualified())
	}
	for _, m := range s.MaterializedViews {
		add(m.Qualified(), m.DependsOn, pos[m.Qualified()], m.Qualified())
	}
	b.checkCycles(nodes)
}

// checkCycles reports a dependency cycle.
//
// The traversal is over a sorted node list and sorted edges, so the cycle it
// names is the same one on every run. Reporting whichever cycle a map iteration
// happened to reach first would make the diagnostic change between runs over
// one unchanged project, which is the kind of instability that teaches people
// to rerun a command until it says something else.
// depNode is one relation and what it waits for. It is internal and stays that
// way: a dependency graph is an implementation detail of ordering, not
// something a project should be able to hold.
type depNode struct {
	name string
	deps []string
}

func (b *builder) checkCycles(nodes []depNode) {
	edges := make(map[string][]string, len(nodes))
	var names []string
	for _, n := range nodes {
		d := slices.Clone(n.deps)
		slices.Sort(d)
		edges[n.name] = d
		names = append(names, n.name)
	}
	slices.Sort(names)

	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(names))
	var stack []string
	var walk func(string) bool
	walk = func(n string) bool {
		colour[n] = grey
		stack = append(stack, n)
		for _, d := range edges[n] {
			switch colour[d] {
			case grey:
				// The cycle is reported from where it closes, so the message
				// reads as the loop somebody wrote.
				at := slices.Index(stack, d)
				loop := append(slices.Clone(stack[at:]), d)
				b.failCode(diag.E029, "these relations depend on each other: %s. "+
					"There is no order they can be created in, so nothing here can be applied",
					strings.Join(loop, " -> "))
				return true
			case white:
				if _, known := edges[d]; known && walk(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
		return false
	}
	for _, n := range names {
		if colour[n] == white {
			if walk(n) {
				return
			}
		}
	}
}
